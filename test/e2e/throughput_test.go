// Throughput measurement harness. Not an assertion suite: it prints numbers.
//
// Run with:
//
//	go test ./test/e2e -run TestThroughput -v -timeout 20m
package e2e_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/quarks-tech/amqpx"

	protocodec "github.com/quarks-tech/protoevent-go/pkg/encoding/proto"
	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/sequence"
	tidbstore "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq"
)

const tpQueue = "tp-consumer"

// requireMeasure gates these harnesses out of `make test`: they publish tens of
// thousands of rows and run for minutes. Run them with
//
//	OUTBOX_MEASURE=1 go test ./test/e2e -run TestThroughput -v -timeout 20m
func requireMeasure(t *testing.T) {
	t.Helper()
	if os.Getenv("OUTBOX_MEASURE") == "" {
		t.Skip("measurement harness: set OUTBOX_MEASURE=1 to run")
	}
}

func tpReset(t *testing.T, ctx context.Context) {
	t.Helper()
	for _, q := range []string{
		"DELETE FROM outbox_messages",
		"DELETE FROM outbox_offsets",
		"DELETE FROM relay_locks",
		"UPDATE outbox_sequencers SET next_seq = 1 WHERE name = 'default'",
	} {
		if _, err := testDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("reset (%s): %v", q, err)
		}
	}
}

func tpMeta() (*event.Metadata, []byte) {
	md := event.NewMetadata(eventType)
	md.ID = uuid.NewString()
	md.Source = "tp"
	md.Subject = "0"
	md.DataContentType = event.ContentType(protocodec.Name)
	md.Time = time.Now().UTC()
	body, _ := proto.Marshal(wrapperspb.String("x"))

	return md, body
}

// tpTopology declares the quorum queue + binding the way an external topology
// manager would: the code under test never creates it.
func tpTopology(t *testing.T) {
	t.Helper()
	testBroker.DeclareExchange(t, serviceName, "topic")
	testBroker.DeclareQueue(t, tpQueue, amqp.Table{"x-queue-type": "quorum"})
	testBroker.BindQueue(t, tpQueue, eventName, serviceName)
}

// TestThroughputSenderCeiling measures the rabbitmq.Sender's SERIAL
// send-and-confirm rate against a real quorum queue — the ceiling the relay's
// single-goroutine drain loop inherits.
func TestThroughputSenderCeiling(t *testing.T) {
	requireMeasure(t)
	if testBroker == nil {
		t.Skip("no Docker")
	}
	ctx := t.Context()
	tpTopology(t)

	cases := []struct {
		name string
		opts []rabbitmq.SenderOption
	}{
		{name: "confirms(default)"},
		{name: "no-confirms", opts: []rabbitmq.SenderOption{rabbitmq.WithoutPublisherConfirms()}},
	}

	const n = 2000

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := amqpx.NewClient(&amqpx.Config{Address: testBroker.Address})
			defer func() { _ = client.Close() }()

			s := rabbitmq.NewSender(client, tc.opts...)
			sd := &eventbus.ServiceDesc{ServiceName: serviceName, Events: []eventbus.EventDesc{{Name: eventName}}}
			if err := s.Setup(ctx, sd); err != nil {
				t.Fatalf("setup: %v", err)
			}

			// warm the channel / confirm handshake
			md, body := tpMeta()
			if err := s.Send(ctx, md, body); err != nil {
				t.Fatalf("warmup send: %v", err)
			}

			start := time.Now()
			for range n {
				md, body := tpMeta()
				if err := s.Send(ctx, md, body); err != nil {
					t.Fatalf("send: %v", err)
				}
			}
			d := time.Since(start)
			t.Logf("SERIAL %s: %d sends in %v = %.0f/s (%.2fms per send)",
				tc.name, n, d.Round(time.Millisecond), float64(n)/d.Seconds(),
				float64(d.Microseconds())/float64(n)/1000)
		})
	}

	testBroker.DeleteQueue(t, tpQueue)
}

// TestThroughputRelayDrain measures the end-to-end sustained drain rate: N rows
// pre-committed to the outbox, one relay, a real quorum queue.
func TestThroughputRelayDrain(t *testing.T) {
	requireMeasure(t)
	if testDB == nil || testBroker == nil {
		t.Skip("no Docker")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	tpReset(t, ctx)
	tpTopology(t)

	const n = 3000

	// Prefill: separate committed transactions, like real publishers.
	fillStart := time.Now()
	var wg sync.WaitGroup
	const writers = 8
	for w := range writers {
		wg.Go(func() {
			for i := w; i < n; i += writers {
				tx, err := testDB.BeginTx(ctx, nil)
				if err != nil {
					t.Errorf("begin: %v", err)

					return
				}
				md, body := tpMeta()
				md.Subject = strconv.Itoa(i)
				if err := outbox.NewSender(tidbstore.NewStore(tx)).Send(ctx, md, body); err != nil {
					_ = tx.Rollback()
					t.Errorf("publish: %v", err)

					return
				}
				if err := tx.Commit(); err != nil {
					t.Errorf("commit: %v", err)

					return
				}
			}
		})
	}
	wg.Wait()
	fillDur := time.Since(fillStart)
	t.Logf("PREFILL: %d rows in %v = %.0f/s (%d concurrent writers)",
		n, fillDur.Round(time.Millisecond), float64(n)/fillDur.Seconds(), writers)

	client := amqpx.NewClient(&amqpx.Config{Address: testBroker.Address})
	defer func() { _ = client.Close() }()

	sender := rabbitmq.NewSender(client)
	sd := &eventbus.ServiceDesc{ServiceName: serviceName, Events: []eventbus.EventDesc{{Name: eventName}}}
	if err := sender.Setup(ctx, sd); err != nil {
		t.Fatalf("sender setup: %v", err)
	}

	var (
		mu          sync.Mutex
		drained     int
		firstDrain  time.Time
		lastDrain   time.Time
		maxOldest   time.Duration
		sweptCalls  int
		sequenced   int
		drainEvents int
	)
	done := make(chan struct{})
	var closeOnce sync.Once

	obs := relay.Observer{
		OnDrained: func(_ string, sent int, oldest time.Duration, more bool) {
			mu.Lock()
			if firstDrain.IsZero() {
				firstDrain = time.Now()
			}
			lastDrain = time.Now()
			drained += sent
			drainEvents++
			if oldest > maxOldest {
				maxOldest = oldest
			}
			reached := drained >= n
			mu.Unlock()
			if reached {
				closeOnce.Do(func() { close(done) })
			}
		},
		OnSwept:     func(string, int) { mu.Lock(); sweptCalls++; mu.Unlock() },
		OnSequenced: func(_ string, k int) { mu.Lock(); sequenced += k; mu.Unlock() },
	}

	r, err := sequence.NewRelay("tp", tidbstore.NewRelayStore(testDB), sender,
		sequence.WithStartFromBeginning(),
		sequence.WithObserver(obs),
	)
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	runCtx, stop := context.WithCancel(ctx)
	relayDone := make(chan error, 1)
	t0 := time.Now()
	go func() { relayDone <- r.Run(runCtx) }()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("timed out: drained %d/%d", drained, n)
	}
	stop()
	<-relayDone

	mu.Lock()
	defer mu.Unlock()
	total := lastDrain.Sub(t0)
	steady := lastDrain.Sub(firstDrain)
	t.Logf("DRAIN: %d events, wall %v = %.0f/s; steady window %v = %.0f/s",
		drained, total.Round(time.Millisecond), float64(drained)/total.Seconds(),
		steady.Round(time.Millisecond), float64(drained)/steady.Seconds())
	t.Logf("  OnDrained calls=%d  OnSequenced total=%d  OnSwept calls=%d  max oldestAge reported=%v",
		drainEvents, sequenced, sweptCalls, maxOldest.Round(time.Millisecond))

	testBroker.DeleteQueue(t, tpQueue)
}

// TestThroughputSequencerContention measures how N relays (each running its own
// sequencer by default) contend on the one pessimistic counter row.
func TestThroughputSequencerContention(t *testing.T) {
	requireMeasure(t)
	if testDB == nil {
		t.Skip("no Docker")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	for _, groups := range []int{1, 2, 4, 8} {
		t.Run(fmt.Sprintf("groups=%d", groups), func(t *testing.T) {
			tpReset(t, ctx)

			const n = 4000
			fillOutbox(t, ctx, n)

			rs := tidbstore.NewRelayStore(testDB)

			var (
				assigned atomic.Int64
				wg       sync.WaitGroup
				errs     atomic.Int64
			)
			start := time.Now()
			runCtx, stop := context.WithCancel(ctx)
			for range groups {
				wg.Go(func() {
					for runCtx.Err() == nil {
						k, err := rs.SequenceMessages(runCtx, 1000)
						if err != nil {
							errs.Add(1)

							return
						}
						if k == 0 {
							return
						}
						if assigned.Add(int64(k)) >= int64(n) {
							return
						}
					}
				})
			}
			wg.Wait()
			stop()
			d := time.Since(start)
			t.Logf("SEQUENCER groups=%d: %d rows in %v = %.0f rows/s, errors=%d",
				groups, assigned.Load(), d.Round(time.Millisecond),
				float64(assigned.Load())/d.Seconds(), errs.Load())
		})
	}
}

func fillOutbox(t *testing.T, ctx context.Context, n int) {
	t.Helper()
	var wg sync.WaitGroup
	const writers = 8
	for w := range writers {
		wg.Go(func() {
			for i := w; i < n; i += writers {
				tx, err := testDB.BeginTx(ctx, nil)
				if err != nil {
					t.Errorf("begin: %v", err)

					return
				}
				md, body := tpMeta()
				if err := outbox.NewSender(tidbstore.NewStore(tx)).Send(ctx, md, body); err != nil {
					_ = tx.Rollback()
					t.Errorf("publish: %v", err)

					return
				}
				if err := tx.Commit(); err != nil {
					t.Errorf("commit: %v", err)

					return
				}
			}
		})
	}
	wg.Wait()
}

var _ = sql.ErrNoRows

// delaySender wraps a Sender with a fixed extra acknowledgement latency, to map the
// drain rate against confirm RTT.
//
// It deliberately does NOT implement eventbus.BatchSender, so a relay over it takes
// the per-message path: the model there is rate = 1/RTT.
type delaySender struct {
	inner eventbus.Sender
	d     time.Duration
}

func (s delaySender) Send(ctx context.Context, md *event.Metadata, data []byte) error {
	if err := sleepCtx(ctx, s.d); err != nil {
		return err
	}

	return s.inner.Send(ctx, md, data)
}

// delayBatchSender is delaySender's overlapped-acknowledgement twin: the same added
// latency, but paid ONCE per batch rather than once per message, which is what
// collecting the confirms together does.
type delayBatchSender struct {
	inner eventbus.BatchSender
	d     time.Duration
}

func (s delayBatchSender) Send(ctx context.Context, md *event.Metadata, data []byte) error {
	if err := sleepCtx(ctx, s.d); err != nil {
		return err
	}

	return s.inner.Send(ctx, md, data)
}

func (s delayBatchSender) SendBatch(ctx context.Context, msgs []eventbus.Outgoing) (int, error) {
	if err := sleepCtx(ctx, s.d); err != nil {
		return 0, err
	}

	return s.inner.SendBatch(ctx, msgs)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
		return ctx.Err()
	}

	return nil
}

// TestThroughputConfirmLatencySensitivity maps the relay's drain rate against
// added per-send latency. A cross-AZ quorum queue's confirm RTT lands in this
// range; loopback Docker's does not.
func TestThroughputConfirmLatencySensitivity(t *testing.T) {
	requireMeasure(t)
	if testDB == nil || testBroker == nil {
		t.Skip("no Docker")
	}

	for _, mode := range []string{"serial", "batched"} {
		for _, extra := range []time.Duration{0, 2 * time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond} {
			t.Run(mode+"/"+extra.String(), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
				defer cancel()

				tpReset(t, ctx)
				tpTopology(t)

				const n = 600
				fillOutbox(t, ctx, n)

				client := amqpx.NewClient(&amqpx.Config{Address: testBroker.Address})
				defer func() { _ = client.Close() }()
				base := rabbitmq.NewSender(client)
				sd := &eventbus.ServiceDesc{ServiceName: serviceName, Events: []eventbus.EventDesc{{Name: eventName}}}
				if err := base.Setup(ctx, sd); err != nil {
					t.Fatalf("setup: %v", err)
				}

				var (
					mu      sync.Mutex
					drained int
					last    time.Time
				)
				done := make(chan struct{})
				var once sync.Once
				obs := relay.Observer{OnDrained: func(_ string, sent int, _ time.Duration, _ bool) {
					mu.Lock()
					drained += sent
					last = time.Now()
					hit := drained >= n
					mu.Unlock()
					if hit {
						once.Do(func() { close(done) })
					}
				}}

				var wrapped eventbus.Sender = delaySender{inner: base, d: extra}
				if mode == "batched" {
					wrapped = delayBatchSender{inner: base, d: extra}
				}

				r, err := sequence.NewRelay("tp", tidbstore.NewRelayStore(testDB),
					wrapped,
					sequence.WithStartFromBeginning(), sequence.WithObserver(obs))
				if err != nil {
					t.Fatalf("NewRelay: %v", err)
				}
				runCtx, stop := context.WithCancel(ctx)
				relayDone := make(chan error, 1)
				t0 := time.Now()
				go func() { relayDone <- r.Run(runCtx) }()

				select {
				case <-done:
				case <-ctx.Done():
					t.Fatalf("timeout: drained %d/%d", drained, n)
				}
				stop()
				<-relayDone

				mu.Lock()
				d := last.Sub(t0)
				mu.Unlock()
				t.Logf("%s added-latency=%v: %d drained in %v = %.0f/s", mode, extra, n,
					d.Round(time.Millisecond), float64(n)/d.Seconds())

				testBroker.DeleteQueue(t, tpQueue)
			})
		}
	}
}

// TestSustainedPublishAndLagSignal runs publishers at a target rate WHILE the
// relay drains, and records what OnDrained's oldestAge reports. It answers two
// things: whether the relay keeps up at that rate, and whether the lag signal
// tracks a growing backlog or reads ~0 through it.
func TestSustainedPublishAndLagSignal(t *testing.T) {
	requireMeasure(t)
	if testDB == nil || testBroker == nil {
		t.Skip("no Docker")
	}

	for _, rate := range []int{300, 800, 2500} {
		t.Run(fmt.Sprintf("rate=%d/s", rate), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
			defer cancel()

			tpReset(t, ctx)
			tpTopology(t)

			client := amqpx.NewClient(&amqpx.Config{Address: testBroker.Address})
			defer func() { _ = client.Close() }()
			sender := rabbitmq.NewSender(client)
			sd := &eventbus.ServiceDesc{ServiceName: serviceName, Events: []eventbus.EventDesc{{Name: eventName}}}
			if err := sender.Setup(ctx, sd); err != nil {
				t.Fatalf("setup: %v", err)
			}

			type sample struct {
				at     time.Duration
				oldest time.Duration
			}
			var (
				mu        sync.Mutex
				drained   int
				samples   []sample
				lastDrain time.Time
			)
			t0 := time.Now()
			obs := relay.Observer{OnDrained: func(_ string, sent int, oldest time.Duration, _ bool) {
				mu.Lock()
				drained += sent
				lastDrain = time.Now()
				samples = append(samples, sample{at: time.Since(t0), oldest: oldest})
				mu.Unlock()
			}}

			r, err := sequence.NewRelay("tp", tidbstore.NewRelayStore(testDB), sender,
				sequence.WithStartFromBeginning(), sequence.WithObserver(obs))
			if err != nil {
				t.Fatalf("NewRelay: %v", err)
			}
			runCtx, stopRelay := context.WithCancel(ctx)
			relayDone := make(chan error, 1)
			go func() { relayDone <- r.Run(runCtx) }()

			// Publish at `rate` for 10 seconds, spread over 8 writers.
			const (
				dur     = 10 * time.Second
				writers = 8
			)
			perWriter := rate / writers
			interval := time.Second / time.Duration(perWriter)
			pubCtx, stopPub := context.WithTimeout(ctx, dur)
			var wg sync.WaitGroup
			var published atomic.Int64
			for range writers {
				wg.Go(func() {
					tick := time.NewTicker(interval)
					defer tick.Stop()
					for {
						select {
						case <-pubCtx.Done():
							return
						case <-tick.C:
						}
						tx, err := testDB.BeginTx(ctx, nil)
						if err != nil {
							return
						}
						md, body := tpMeta()
						if err := outbox.NewSender(tidbstore.NewStore(tx)).Send(ctx, md, body); err != nil {
							_ = tx.Rollback()

							continue
						}
						if tx.Commit() == nil {
							published.Add(1)
						}
					}
				})
			}
			wg.Wait()
			stopPub()
			pubEnd := time.Since(t0)

			// Let the relay try to catch up for up to 30s.
			deadline := time.After(30 * time.Second)
			for {
				mu.Lock()
				caught := drained >= int(published.Load())
				mu.Unlock()
				if caught {
					break
				}
				select {
				case <-deadline:
					mu.Lock()
					t.Logf("  did NOT catch up within 30s after publishing stopped: drained %d of %d",
						drained, published.Load())
					mu.Unlock()

					goto report
				case <-time.After(200 * time.Millisecond):
				}
			}
		report:
			stopRelay()
			<-relayDone

			mu.Lock()
			defer mu.Unlock()
			var maxOldest, oldestAtPubEnd time.Duration
			zeroAfterStart := 0
			for _, s := range samples {
				if s.oldest > maxOldest {
					maxOldest = s.oldest
				}
				if s.at <= pubEnd {
					oldestAtPubEnd = s.oldest
				}
				if s.oldest == 0 {
					zeroAfterStart++
				}
			}
			drainWindow := lastDrain.Sub(t0)
			t.Logf("rate=%d/s for %v: published=%d in %v (%.0f/s achieved); drained=%d over %v (%.0f/s)",
				rate, dur, published.Load(), pubEnd.Round(time.Millisecond),
				float64(published.Load())/dur.Seconds(),
				drained, drainWindow.Round(time.Millisecond), float64(drained)/drainWindow.Seconds())
			t.Logf("  oldestAge: max=%v at-publish-end=%v  zero-valued samples=%d/%d",
				maxOldest.Round(time.Millisecond), oldestAtPubEnd.Round(time.Millisecond),
				zeroAfterStart, len(samples))

			testBroker.DeleteQueue(t, tpQueue)
		})
	}
}
