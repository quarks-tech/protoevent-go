// Package e2e_test drives the whole delivery chain with nothing faked: a business
// transaction writes its row and an outbox row atomically, a real sequencer assigns
// seq, a real relay drains to a real RabbitMQ broker, and a real subscriber's handler
// records what it saw.
//
// It exists because every other delivery assertion in this repo stops one hop short.
// The store suites assert on rows; the relay suites assert on a recordingSender that
// cannot drop anything; the broker suite publishes without a store. None of them can
// observe a SILENT GAP — an event the relay legitimately reported as delivered that
// never reaches a consumer — which is the one failure mode with no natural alarm:
// no error, no stall, OnDrained reporting healthy throughput, and the row eventually
// swept as consumed.
package e2e_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/quarks-tech/amqpx"

	protocodec "github.com/quarks-tech/protoevent-go/pkg/encoding/proto"
	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/sequence"
	tidbstore "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb/tidbtest"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/rabbitmqtest"
)

const (
	// serviceName is the exchange; eventName the routing key. event.SplitType splits
	// Metadata.Type at the LAST dot, so the type is serviceName + "." + eventName.
	serviceName = "e2e.v1"
	eventName   = "Thing"
	eventType   = serviceName + "." + eventName
)

var (
	testDB     *sql.DB
	testBroker *rabbitmqtest.Instance
)

// TestMain boots both dependencies. Either being unavailable skips the suite, on the
// same "no Docker locally, mandatory in CI" rule the store modules use.
func TestMain(m *testing.M) {
	ctx := context.Background()

	// No defer for the teardowns: every exit from TestMain goes through os.Exit (or
	// log.Fatalf), which does NOT run deferred functions — a deferred Terminate here
	// would silently leak a container instead of cleaning one up. Each exit path stops
	// exactly what it has started.
	tidbInst, stopTiDB, err := tidbtest.Start(ctx)
	if err != nil {
		if errors.Is(err, tidbtest.ErrDockerUnavailable) {
			if os.Getenv("CI") != "" {
				log.Fatalf("e2e tests require Docker in CI: %v", err)
			}
			fmt.Fprintf(os.Stderr, "no Docker: e2e tests skipped: %v\n", err)
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "e2e tidb setup: %v\n", err)
		os.Exit(1)
	}

	brokerInst, stopBroker, err := rabbitmqtest.Start(ctx)
	if err != nil {
		stopTiDB()
		if errors.Is(err, rabbitmqtest.ErrDockerUnavailable) {
			if os.Getenv("CI") != "" {
				log.Fatalf("e2e tests require Docker in CI: %v", err)
			}
			fmt.Fprintf(os.Stderr, "no Docker: e2e tests skipped: %v\n", err)
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "e2e rabbitmq setup: %v\n", err)
		os.Exit(1)
	}

	testDB = tidbInst.DB
	testBroker = brokerInst

	code := m.Run()

	stopBroker()
	stopTiDB()
	os.Exit(code)
}

// publishInBusinessTx writes a business row and its outbox row in ONE transaction,
// which is the whole point of an outbox. ordinal is carried in Subject so the
// consumer can reconstruct the order it observed.
func publishInBusinessTx(t *testing.T, ctx context.Context, ordinal int) string {
	t.Helper()

	tx, err := testDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		"INSERT INTO e2e_business (ordinal) VALUES (?)", ordinal); err != nil {
		t.Fatalf("business write %d: %v", ordinal, err)
	}

	md := event.NewMetadata(eventType)
	// A real UUID, because the TiDB store keys rows on event_id BINARY(16) and the
	// default ReuseMetadataID generator uses Metadata.ID as that key. event.NewMetadata
	// deliberately leaves ID empty — a real publisher gets one from eventbus.Publisher —
	// and both the empty and the non-UUID case are rejected with an actionable error
	// rather than papered over. The ordinal travels in Subject instead.
	md.ID = uuid.NewString()
	md.Source = "e2e"
	md.Subject = strconv.Itoa(ordinal)
	// "application/proto", NOT "application/protobuf". The registry holds exactly the
	// subtypes "proto" and "json" (encoding/proto.Name, encoding/json.Name), so
	// "application/protobuf" — the spelling most CloudEvents stacks use, and the one
	// several tests in this repo set — resolves to NO codec and every delivery is
	// rejected as unprocessable. Nothing noticed because no test ever decoded a
	// payload; this one does, below.
	md.DataContentType = event.ContentType(protocodec.Name)
	md.Time = time.Now().UTC()

	// A REAL proto payload, so the delivery path exercises the codec end to end
	// rather than smuggling the ordinal through Subject.
	body, err := proto.Marshal(wrapperspb.String(strconv.Itoa(ordinal)))
	if err != nil {
		t.Fatalf("marshal payload %d: %v", ordinal, err)
	}

	if err := outbox.NewSender(tidbstore.NewStore(tx)).Send(ctx, md, body); err != nil {
		t.Fatalf("publish %d: %v", ordinal, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit %d: %v", ordinal, err)
	}

	return md.ID
}

// TestNoSilentGapFromBusinessTxToConsumer is the gap detector.
//
// It publishes a run of events in separate, sequentially committed business
// transactions — so begin order equals commit order and seq order is unambiguous —
// then drives a real relay into a real broker and asserts on what a real handler saw:
//
//   - every published event arrives (no gap: THE assertion, since a missing event
//     produces no error anywhere);
//   - first occurrences are strictly ascending (order preserved end to end);
//   - duplicates, if any, are permitted — the contract is at-least-once — but are
//     reported so an unexpected count is visible rather than silently absorbed.
func TestNoSilentGapFromBusinessTxToConsumer(t *testing.T) {
	if testDB == nil || testBroker == nil {
		t.Skip("no Docker")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	const (
		total    = 50
		consumer = "e2e-consumer"
	)

	if _, err := testDB.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS e2e_business (ordinal BIGINT NOT NULL PRIMARY KEY)"); err != nil {
		t.Fatalf("create business table: %v", err)
	}
	for _, q := range []string{
		"DELETE FROM e2e_business", "DELETE FROM outbox_messages",
		"DELETE FROM outbox_offsets", "DELETE FROM relay_locks",
		"UPDATE outbox_sequencers SET next_seq = 1 WHERE name = 'default'",
	} {
		if _, err := testDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("reset (%s): %v", q, err)
		}
	}

	client := amqpx.NewClient(&amqpx.Config{Address: testBroker.Address})
	defer func() { _ = client.Close() }()

	sd := &eventbus.ServiceDesc{ServiceName: serviceName, Events: []eventbus.EventDesc{{Name: eventName}}}

	sender := rabbitmq.NewSender(client)
	if err := sender.Setup(ctx, sd); err != nil {
		t.Fatalf("sender setup: %v", err)
	}

	var (
		mu       sync.Mutex
		observed []int
	)
	seen := make(chan struct{}, total*4)

	sub := eventbus.NewSubscriber(consumer)
	sub.RegisterHandler(sd, eventName,
		func(_ context.Context, _ *event.Metadata, dec func(any) error, _ eventbus.SubscriberInterceptor) error {
			// Decode through the real registry: this is the only place in the repo
			// that exercises content-type -> GetCodec -> Unmarshal on a delivered
			// event, so a codec or content-type regression surfaces here.
			var payload wrapperspb.StringValue
			if err := dec(&payload); err != nil {
				return err
			}
			ordinal, err := strconv.Atoi(payload.GetValue())
			if err != nil {
				return eventbus.NewUnprocessableEventError(err)
			}
			mu.Lock()
			observed = append(observed, ordinal)
			mu.Unlock()
			select {
			case seen <- struct{}{}:
			default:
			}

			return nil
		})

	// Prefetch 1: with a larger prefetch a requeue can interleave deliveries, which
	// is a property of the CONSUMER's concurrency, not of the log. This test is about
	// the log's guarantee reaching a consumer, so the last hop is kept in order
	// deliberately — see the ordering caveat in the root README.
	receiver := rabbitmq.NewReceiver(client,
		rabbitmq.WithTopologySetup(), rabbitmq.WithPrefetchCount(1))

	subCtx, stopSub := context.WithCancel(ctx)
	defer stopSub()
	subDone := make(chan error, 1)
	go func() { subDone <- sub.Subscribe(subCtx, receiver) }()

	// The queue must exist before publishing, or the relay's publishes are
	// unroutable — which by default the broker acks and discards silently. That is a
	// real hazard (see rabbitmq.WithMandatoryPublish) but not the one under test.
	testBroker.WaitForQueue(t, consumer)

	published := make([]string, 0, total)
	for i := range total {
		published = append(published, publishInBusinessTx(t, ctx, i))
	}

	relay, err := sequence.NewRelay("e2e", tidbstore.NewRelayStore(testDB), sender,
		sequence.WithStartFromBeginning(),
		sequence.WithBatchSize(10),
		sequence.WithPollInterval(200*time.Millisecond),
		sequence.WithLeaseTTL(30*time.Second),
		sequence.WithOpTimeout(10*time.Second),
	)
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	relayCtx, stopRelay := context.WithCancel(ctx)
	relayDone := make(chan error, 1)
	go func() { relayDone <- relay.Run(relayCtx) }()

	// Wait for the consumer to have seen every ordinal at least once.
	deadline := time.After(2 * time.Minute)
	for {
		mu.Lock()
		distinct := len(firstOccurrences(observed))
		mu.Unlock()
		if distinct >= total {
			break
		}
		select {
		case <-seen:
		case <-deadline:
			mu.Lock()
			got := slices.Clone(observed)
			mu.Unlock()
			stopRelay()
			<-relayDone
			stopSub()
			<-subDone
			t.Fatalf("only %d/%d distinct events reached the consumer: missing %v.\n"+
				"Nothing reported an error — this is the silent-gap failure mode. observed=%v",
				len(firstOccurrences(got)), total, missing(got, total), got)
		}
	}

	stopRelay()
	<-relayDone
	stopSub()
	<-subDone

	mu.Lock()
	got := slices.Clone(observed)
	mu.Unlock()

	if m := missing(got, total); len(m) != 0 {
		t.Fatalf("events never delivered: %v (published %d)", m, len(published))
	}

	// First occurrences must be ascending: that is the log's order surviving to a
	// handler.
	firsts := firstOccurrences(got)
	if !slices.IsSorted(firsts) {
		t.Fatalf("consumer observed events out of order.\nfirst occurrences: %v\nfull sequence: %v",
			firsts, got)
	}

	if dupes := len(got) - len(firstOccurrences(got)); dupes > 0 {
		t.Logf("at-least-once: %d duplicate deliveries (permitted by contract)", dupes)
	}
}

// missing reports ordinals in [0,total) that never arrived.
func missing(xs []int, total int) []int {
	set := make(map[int]struct{}, len(xs))
	for _, x := range xs {
		set[x] = struct{}{}
	}
	var out []int
	for i := range total {
		if _, ok := set[i]; !ok {
			out = append(out, i)
		}
	}

	return out
}

// firstOccurrences returns each ordinal's first sighting, in observation order.
func firstOccurrences(xs []int) []int {
	seen := make(map[int]struct{}, len(xs))
	out := make([]int, 0, len(xs))
	for _, x := range xs {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}

	return out
}
