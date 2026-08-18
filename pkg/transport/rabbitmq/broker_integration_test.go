package rabbitmq_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/amqpx"
	"github.com/quarks-tech/amqpx/connpool"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/parkinglot"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/rabbitmqtest"
)

// broker lazily boots the shared ephemeral RabbitMQ, once per test binary.
//
// LAZY, not a TestMain: receiver_internal_test.go and sender_internal_test.go compile
// into this same binary, and TestMain is per-binary — so starting the container there
// made every pure unit test (and every `go test -run TestSomeUnitTest`) pay a Docker
// probe and a broker boot for a package whose unit tests otherwise finish in well under
// a second. Booting on first use keeps the cost on the tests that actually need a
// broker.
var broker = sync.OnceValues(func() (*rabbitmqtest.Instance, error) {
	inst, cleanup, err := rabbitmqtest.Start(context.Background())
	if err != nil {
		return nil, err
	}
	// Terminated by the process exiting. There is no TestMain to hang a teardown on,
	// and testcontainers' Ryuk reaps the container regardless — which is also what
	// happens today on any os.Exit path.
	_ = cleanup

	return inst, nil
})

// requireBroker returns the shared broker, skipping (or failing in CI) when Docker is
// unavailable — the same policy the tidb and mongodb suites apply in their TestMain.
func requireBroker(t *testing.T) *rabbitmqtest.Instance {
	t.Helper()

	inst, err := broker()
	if err != nil {
		if errors.Is(err, rabbitmqtest.ErrDockerUnavailable) {
			if os.Getenv("CI") != "" {
				t.Fatalf("rabbitmq integration tests require Docker in CI: %v", err)
			}
			t.Skipf("no Docker: %v", err)
		}
		t.Fatalf("rabbitmq integration setup: %v", err)
	}

	return inst
}

// newTestClient dials the shared broker and closes the client when the test ends.
func newTestClient(t *testing.T) *amqpx.Client {
	t.Helper()
	inst := requireBroker(t)
	c := amqpx.NewClient(&amqpx.Config{Address: inst.Address})
	t.Cleanup(func() { _ = c.Close() })

	return c
}

// testEventName is the single event these fixtures publish.
const testEventName = "Thing"

// testEvent builds sendable metadata for the given service's fixture event.
func testEvent(service string) *event.Metadata {
	md := event.NewMetadata(service + "." + testEventName)
	md.ID = "id-" + testEventName
	md.Time = time.Now().UTC()
	md.DataContentType = "application/protobuf"

	return md
}

// TestTransientHandlerErrorDoesNotDestroyTheEvent is the broker-backed proof of the
// default receiver's per-delivery policy.
//
// A handler returning a plain error — a database timeout, a downstream 503, the
// most ordinary failure a consumer has — used to reach Reject(requeue=false) on a
// queue with no dead-letter exchange, and RabbitMQ then DISCARDS the message. The
// whole outbox exists to guarantee the event reaches the broker; the library's own
// default consumer threw it away on the first blip, with only a log line.
//
// The delivery must survive a transient error. This test asserts it by observing a
// redelivery: if the broker still has the message, it comes back.
func TestTransientHandlerErrorDoesNotDestroyTheEvent(t *testing.T) {
	client := newTestClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	const (
		service   = "reject.v1"
		eventName = "Thing"
		consumer  = "reject-consumer"
	)

	sd := &eventbus.ServiceDesc{ServiceName: service, Events: []eventbus.EventDesc{{Name: eventName}}}

	sender := rabbitmq.NewSender(client)
	if err := sender.Setup(ctx, sd); err != nil {
		t.Fatalf("sender setup: %v", err)
	}

	var (
		mu       sync.Mutex
		attempts int
	)
	redelivered := make(chan struct{})

	sub := eventbus.NewSubscriber(consumer)
	sub.RegisterHandler(sd, eventName,
		func(context.Context, *event.Metadata, func(any) error, eventbus.SubscriberInterceptor) error {
			mu.Lock()
			attempts++
			n := attempts
			mu.Unlock()
			if n >= 2 {
				select {
				case <-redelivered:
				default:
					close(redelivered)
				}
			}

			// A transient fault, NOT an unprocessable event: retrying can succeed.
			return errors.New("downstream timeout")
		})

	receiver := rabbitmq.NewReceiver(client, rabbitmq.WithTopologySetup())

	subCtx, stopSub := context.WithCancel(ctx)
	defer stopSub()
	done := make(chan error, 1)
	go func() { done <- sub.Subscribe(subCtx, receiver) }()

	// The queue and its binding exist only once Subscribe has run Setup.
	requireBroker(t).WaitForQueue(t, consumer)

	if err := sender.Send(ctx, testEvent(service), []byte("payload")); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case <-redelivered:
	case <-time.After(15 * time.Second):
		mu.Lock()
		n := attempts
		mu.Unlock()
		stopSub()
		<-done
		t.Fatalf("handler ran %d time(s) and the event was never redelivered: a transient error "+
			"destroyed it (Reject with requeue=false and no dead-letter exchange discards the "+
			"message at the broker)", n)
	}

	stopSub()
	<-done
}

// TestUnroutablePublishIsDetectedWithMandatory is the broker-backed proof that
// publisher confirms do not cover unroutable publishes.
//
// RabbitMQ acks a publish once the exchange has determined its routing set,
// INCLUDING when that set is empty. So with confirms alone — the default — Send
// returns nil for an event no queue received, and an outbox relay commits its
// offset (or resume token) past it while OnDrained counts it as sent. No infra
// failure is needed: a new event type published before its consumer's binding
// exists, a binding dropped during a topology migration, or a routing-key typo all
// produce it.
//
// Both halves are asserted here, because the default is a deliberate trade and has
// to stay documented by a test rather than by a comment alone.
func TestUnroutablePublishIsDetectedWithMandatory(t *testing.T) {
	client := newTestClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// An exchange with NO queue bound to it: exactly the state of an event type
	// whose consumer has not been deployed yet.
	const service = "unroutable.v1"
	sd := &eventbus.ServiceDesc{ServiceName: service, Events: []eventbus.EventDesc{{Name: testEventName}}}

	t.Run("default reports success for an event no queue received", func(t *testing.T) {
		sender := rabbitmq.NewSender(client)
		if err := sender.Setup(ctx, sd); err != nil {
			t.Fatalf("sender setup: %v", err)
		}

		if err := sender.Send(ctx, testEvent(service), []byte("payload")); err != nil {
			t.Fatalf("Send = %v; the default is documented to ACCEPT an unroutable publish, so if "+
				"this now fails the default changed and WithMandatoryPublish is redundant", err)
		}
	})

	t.Run("WithMandatoryPublish reports it", func(t *testing.T) {
		sender := rabbitmq.NewSender(client, rabbitmq.WithMandatoryPublish())
		if err := sender.Setup(ctx, sd); err != nil {
			t.Fatalf("sender setup: %v", err)
		}

		err := sender.Send(ctx, testEvent(service), []byte("payload"))
		if err == nil {
			t.Fatal("Send = nil for a publish no queue received: the relay would commit its " +
				"position past an event the broker discarded")
		}
		if !errors.Is(err, rabbitmq.ErrUnroutable) {
			t.Fatalf("Send = %v, want it to wrap ErrUnroutable so a caller can classify it", err)
		}
	})

	t.Run("a routable publish still succeeds under mandatory", func(t *testing.T) {
		// Same exchange, but now with a bound queue: the option must not turn a
		// perfectly good publish into an error (a stale basic.return leaking onto the
		// next publish would do exactly that).
		const consumer = "unroutable-consumer"
		sub := eventbus.NewSubscriber(consumer)
		sub.RegisterHandler(sd, testEventName,
			func(context.Context, *event.Metadata, func(any) error, eventbus.SubscriberInterceptor) error {
				return nil
			})
		receiver := rabbitmq.NewReceiver(client, rabbitmq.WithTopologySetup())

		subCtx, stopSub := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- sub.Subscribe(subCtx, receiver) }()
		requireBroker(t).WaitForQueue(t, consumer)

		sender := rabbitmq.NewSender(client, rabbitmq.WithMandatoryPublish())
		if err := sender.Setup(ctx, sd); err != nil {
			t.Fatalf("sender setup: %v", err)
		}
		// Several in a row: the first publish's return (if any leaked) must not be
		// attributed to a later one.
		for i := range 3 {
			if err := sender.Send(ctx, testEvent(service), []byte("payload")); err != nil {
				t.Fatalf("Send %d to a bound exchange = %v, want nil", i, err)
			}
		}

		stopSub()
		<-done
	})
}

// TestParkedMessageSurvivesAMissingParkingLotBinding is the broker-backed proof for
// the one place in this library where a message exists ONLY on the wire.
//
// putIntoParkingLot publishes the poison delivery to the DLX with routing key
// "parkingLot" and then ACKS the original, destroying the last copy. That publish
// used mandatory=false, and RabbitMQ acks a publish whose routing set is empty — so
// if the .pl queue's binding is absent, the broker discards the copy, the ack
// destroys the original, and the message is gone from every queue with no error, no
// log, and Subscribe returning nil.
//
// The in-code justification for mandatory=false was "a missing binding is a topology
// error that WithTopologySetup prevents". It does not: Setup runs ONCE per process,
// so this test uses the fully documented WithTopologySetup()+WithBindingsSetup()
// configuration, lets Setup declare everything correctly, and then deletes the .pl
// queue the way an operator, a topology migration, or a botched terraform apply
// would. Restarting re-declares the binding; the messages lost in the window do not
// come back.
//
// The message must end up SOMEWHERE. Requeued-and-retried is a fine outcome (it is
// what the existing !acked branch already chooses); destroyed is not.
func TestParkedMessageSurvivesAMissingParkingLotBinding(t *testing.T) {
	client := newTestClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	const (
		service  = "parkdrift.v1"
		consumer = "parkdrift-consumer"
	)
	parkingLotQueue := consumer + ".pl"

	sd := &eventbus.ServiceDesc{ServiceName: service, Events: []eventbus.EventDesc{{Name: testEventName}}}

	sender := rabbitmq.NewSender(client)
	if err := sender.Setup(ctx, sd); err != nil {
		t.Fatalf("sender setup: %v", err)
	}

	handled := make(chan struct{}, 4)
	sub := eventbus.NewSubscriber(consumer)
	sub.RegisterHandler(sd, testEventName,
		func(context.Context, *event.Metadata, func(any) error, eventbus.SubscriberInterceptor) error {
			select {
			case handled <- struct{}{}:
			default:
			}

			// Permanently unprocessable: this is the delivery the parking lot exists for.
			return eventbus.NewUnprocessableEventError(errors.New("poison payload"))
		})

	receiver := parkinglot.NewReceiver(client,
		parkinglot.WithTopologySetup(), parkinglot.WithBindingsSetup())

	subCtx, stopSub := context.WithCancel(ctx)
	defer stopSub()
	done := make(chan error, 1)
	go func() { done <- sub.Subscribe(subCtx, receiver) }()

	// Let Setup declare the whole topology correctly first.
	requireBroker(t).WaitForQueue(t, consumer)
	requireBroker(t).WaitForQueue(t, parkingLotQueue)

	// Topology drift: the parking-lot queue (and with it its binding) is removed
	// while the consumer runs.
	requireBroker(t).DeleteQueue(t, parkingLotQueue)

	if err := sender.Send(ctx, testEvent(service), []byte("payload")); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case <-handled:
	case <-time.After(30 * time.Second):
		stopSub()
		<-done
		t.Fatal("handler never ran; the test never reached the parking-lot path")
	}

	// Give the park attempt time to complete either way, then stop consuming so
	// nothing is held unacked while we count.
	stopSub()
	<-done

	// Re-declare so we can count what the broker kept. A message that was requeued
	// sits on the incoming queue; a parked one would be in .pl (which we deleted, so
	// re-parking after a restart is the recovery path).
	incoming, _ := requireBroker(t).QueueDepth(t, consumer)
	if incoming == 0 {
		t.Fatalf("the poison message is on no queue: the park publish was unroutable, the broker "+
			"discarded it, and Ack destroyed the original. incoming=%d — an unroutable park must "+
			"NOT be acknowledged", incoming)
	}
}

// TestParkedMessageReachesTheParkingLot is the control for the drift test above: with
// the topology intact, a poison delivery must actually land in the parking lot.
//
// It exists because the fix for the drift case turned mandatory ON, and the failure
// mode of that change is a FALSE unroutable — a stale basic.return from an earlier
// publish attributed to this one would requeue a park that was in fact routed,
// looping a message that should have been parked once. Several parks in a row are
// exercised for exactly that reason.
func TestParkedMessageReachesTheParkingLot(t *testing.T) {
	client := newTestClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	const (
		service  = "parkok.v1"
		consumer = "parkok-consumer"
	)
	parkingLotQueue := consumer + ".pl"

	sd := &eventbus.ServiceDesc{ServiceName: service, Events: []eventbus.EventDesc{{Name: testEventName}}}

	sender := rabbitmq.NewSender(client)
	if err := sender.Setup(ctx, sd); err != nil {
		t.Fatalf("sender setup: %v", err)
	}

	const want = 3
	handled := make(chan struct{}, 16)
	sub := eventbus.NewSubscriber(consumer)
	sub.RegisterHandler(sd, testEventName,
		func(context.Context, *event.Metadata, func(any) error, eventbus.SubscriberInterceptor) error {
			select {
			case handled <- struct{}{}:
			default:
			}

			return eventbus.NewUnprocessableEventError(errors.New("poison payload"))
		})

	receiver := parkinglot.NewReceiver(client,
		parkinglot.WithTopologySetup(), parkinglot.WithBindingsSetup())

	subCtx, stopSub := context.WithCancel(ctx)
	defer stopSub()
	done := make(chan error, 1)
	go func() { done <- sub.Subscribe(subCtx, receiver) }()

	requireBroker(t).WaitForQueue(t, consumer)
	requireBroker(t).WaitForQueue(t, parkingLotQueue)

	for range want {
		if err := sender.Send(ctx, testEvent(service), []byte("payload")); err != nil {
			t.Fatalf("send: %v", err)
		}
	}

	for range want {
		select {
		case <-handled:
		case <-time.After(30 * time.Second):
			stopSub()
			<-done
			t.Fatal("handlers did not run for every published event")
		}
	}

	stopSub()
	<-done

	// Poll: the park publish is confirmed before the original is acked, but the
	// broker's queue counters settle asynchronously.
	deadline := time.Now().Add(20 * time.Second)
	var parked int
	for time.Now().Before(deadline) {
		parked, _ = requireBroker(t).QueueDepth(t, parkingLotQueue)
		if parked >= want {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if parked != want {
		t.Fatalf("parking lot holds %d messages, want %d: with the binding intact every poison "+
			"delivery must be parked exactly once — a shortfall means parks are being lost, a "+
			"surplus means a routed park was misread as unroutable and requeued", parked, want)
	}
	if remaining, _ := requireBroker(t).QueueDepth(t, consumer); remaining != 0 {
		t.Fatalf("incoming queue holds %d messages, want 0: a successfully parked delivery must be "+
			"acknowledged away", remaining)
	}
}

// TestPublishStarvedByAConsumerOnTheSameClient pins the pool-starvation landmine and
// the diagnosis it must carry.
//
// A running subscription holds an EXCLUSIVE pool connection for its entire life
// (Receive -> amqpx ConsumeWithDrain -> ProcessWithDrain -> withConn). amqpx defaults
// PoolSize to runtime.GOMAXPROCS(0), which on Go 1.25+ is cgroup-aware — so a 1-CPU
// pod gets a pool of ONE. A service that both subscribes and publishes through a
// single client then fails EVERY publish, permanently, and that is the most common
// shape in this library's own README examples. Two subscriptions need PoolSize >= 3
// before a publish can ever succeed.
//
// This cannot be guarded at construction: amqpx.Client exposes no way to read
// PoolSize (its only exported methods are Process, ProcessWithDrain and Close). So the
// library's obligation is to make the failure legible — the raw error names a
// connection pool and says nothing about the consumer that occupied it, or about the
// knob that fixes it.
func TestPublishStarvedByAConsumerOnTheSameClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	const (
		service  = "starve.v1"
		consumer = "starve-consumer"
	)
	sd := &eventbus.ServiceDesc{ServiceName: service, Events: []eventbus.EventDesc{{Name: testEventName}}}

	// One connection, and a short pool timeout so the failure is quick rather than
	// hidden behind the 1s default.
	client := amqpx.NewClient(&amqpx.Config{
		Address:     requireBroker(t).Address,
		PoolSize:    1,
		PoolTimeout: 500 * time.Millisecond,
	})
	defer func() { _ = client.Close() }()

	sender := rabbitmq.NewSender(client)
	if err := sender.Setup(ctx, sd); err != nil {
		t.Fatalf("sender setup: %v", err)
	}

	// Baseline: with the pool free, publishing works.
	if err := sender.Send(ctx, testEvent(service), []byte("payload")); err != nil {
		t.Fatalf("Send with a free pool = %v, want nil", err)
	}

	handled := make(chan struct{}, 4)
	sub := eventbus.NewSubscriber(consumer)
	sub.RegisterHandler(sd, testEventName,
		func(context.Context, *event.Metadata, func(any) error, eventbus.SubscriberInterceptor) error {
			select {
			case handled <- struct{}{}:
			default:
			}

			return nil
		})
	receiver := rabbitmq.NewReceiver(client, rabbitmq.WithTopologySetup())

	subCtx, stopSub := context.WithCancel(ctx)
	defer stopSub()
	done := make(chan error, 1)
	go func() { done <- sub.Subscribe(subCtx, receiver) }()
	requireBroker(t).WaitForQueue(t, consumer)

	// A declared queue only proves Setup ran, and Setup borrows the pool briefly.
	// The exclusive lease is taken by Receive, so the barrier has to be an actually
	// DELIVERED event. It is published from a second client with its own pool, because
	// publishing from the starved one is the thing under test.
	barrier := amqpx.NewClient(&amqpx.Config{Address: requireBroker(t).Address, PoolSize: 1})
	defer func() { _ = barrier.Close() }()
	barrierSender := rabbitmq.NewSender(barrier)
	if err := barrierSender.Setup(ctx, sd); err != nil {
		t.Fatalf("barrier sender setup: %v", err)
	}
	if err := barrierSender.Send(ctx, testEvent(service), []byte("payload")); err != nil {
		t.Fatalf("barrier send: %v", err)
	}
	select {
	case <-handled:
	case <-time.After(30 * time.Second):
		stopSub()
		<-done
		t.Fatal("the subscription never delivered an event, so it never took its pool lease")
	}

	// The consumer now demonstrably holds the only pool connection.
	err := sender.Send(ctx, testEvent(service), []byte("payload"))
	if err == nil {
		stopSub()
		<-done
		t.Fatal("Send succeeded while a subscription held the only pool connection: if amqpx no " +
			"longer holds an exclusive lease for the life of a consumer, this test and the " +
			"pool-sizing guidance in the READMEs can both be relaxed — verify before deleting")
	}

	// It must be recognizable as pool exhaustion...
	if !errors.Is(err, connpool.ErrPoolTimeout) {
		t.Fatalf("Send error = %v, want it to wrap connpool.ErrPoolTimeout", err)
	}
	// ...and it must say what actually happened. The bare amqpx error names a
	// connection pool; an operator needs to know a CONSUMER took the slot and that
	// PoolSize is the knob.
	for _, want := range []string{"PoolSize", "subscription"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Send error = %q\ndoes not mention %q: pool exhaustion caused by a "+
				"subscription on the same client is indistinguishable from broker trouble, and "+
				"the error names neither the cause nor the remedy", err.Error(), want)
		}
	}

	stopSub()
	<-done

	// The remedy must work: one slot for the consumer, one for publishing.
	roomy := amqpx.NewClient(&amqpx.Config{
		Address:     requireBroker(t).Address,
		PoolSize:    2,
		PoolTimeout: 500 * time.Millisecond,
	})
	defer func() { _ = roomy.Close() }()

	sender2 := rabbitmq.NewSender(roomy)
	if err := sender2.Setup(ctx, sd); err != nil {
		t.Fatalf("roomy sender setup: %v", err)
	}
	sub2 := eventbus.NewSubscriber(consumer)
	sub2.RegisterHandler(sd, testEventName,
		func(context.Context, *event.Metadata, func(any) error, eventbus.SubscriberInterceptor) error {
			return nil
		})
	sub2Ctx, stopSub2 := context.WithCancel(ctx)
	defer stopSub2()
	done2 := make(chan error, 1)
	go func() { done2 <- sub2.Subscribe(sub2Ctx, rabbitmq.NewReceiver(roomy, rabbitmq.WithTopologySetup())) }()
	requireBroker(t).WaitForQueue(t, consumer)

	if err := sender2.Send(ctx, testEvent(service), []byte("payload")); err != nil {
		t.Fatalf("Send with PoolSize 2 alongside one subscription = %v, want nil: the documented "+
			"remedy (PoolSize >= subscriptions + 1) must actually work", err)
	}

	stopSub2()
	<-done2
}

// dedicatedBroker boots a broker used by ONE test, for tests that put the broker
// into a state the shared instance must not be left in — a stopped app, a resource
// alarm, a lowered max_message_size.
func dedicatedBroker(t *testing.T) *rabbitmqtest.Instance {
	t.Helper()

	inst, cleanup, err := rabbitmqtest.Start(context.Background())
	if err != nil {
		if errors.Is(err, rabbitmqtest.ErrDockerUnavailable) {
			if os.Getenv("CI") != "" {
				t.Fatalf("rabbitmq integration tests require Docker in CI: %v", err)
			}
			t.Skipf("no Docker: %v", err)
		}
		t.Fatalf("start dedicated broker: %v", err)
	}
	t.Cleanup(cleanup)

	return inst
}

// TestSubscriptionDoesNotSurviveABrokerRestart documents what a rolling broker
// upgrade does to a subscriber.
//
// The connection dies, amqpx classifies the dropped delivery channel as retryable
// and re-subscribes — but the retry budget is MaxRetries=3 between 8ms and 512ms,
// well under two seconds in total. A broker that is away for longer exhausts it,
// Receive returns, Subscribe returns, and nothing in this library re-subscribes. The
// process then stays alive and healthy, serving traffic and consuming nothing, until
// somebody restarts it.
//
// This test pins that as the CURRENT contract rather than asserting the recovery the
// library does not implement: the failure is silent today, and a test that names it
// is what makes a future auto-resubscribe (or a documented supervision requirement)
// a deliberate change instead of a surprise. The assertion to care about is that
// Subscribe RETURNS — a caller can supervise a returned error, but cannot supervise
// a goroutine that sits there consuming nothing.
func TestSubscriptionDoesNotSurviveABrokerRestart(t *testing.T) {
	inst := dedicatedBroker(t)

	client := amqpx.NewClient(&amqpx.Config{Address: inst.Address})
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()

	const (
		service  = "restart.v1"
		consumer = "restart-consumer"
	)

	sd := &eventbus.ServiceDesc{ServiceName: service, Events: []eventbus.EventDesc{{Name: testEventName}}}

	sender := rabbitmq.NewSender(client)
	if err := sender.Setup(ctx, sd); err != nil {
		t.Fatalf("sender setup: %v", err)
	}

	delivered := make(chan struct{}, 8)
	sub := eventbus.NewSubscriber(consumer)
	sub.RegisterHandler(sd, testEventName,
		func(context.Context, *event.Metadata, func(any) error, eventbus.SubscriberInterceptor) error {
			delivered <- struct{}{}

			return nil
		})

	receiver := rabbitmq.NewReceiver(client, rabbitmq.WithTopologySetup())

	subCtx, stopSub := context.WithCancel(ctx)
	defer stopSub()
	done := make(chan error, 1)
	go func() { done <- sub.Subscribe(subCtx, receiver) }()

	inst.WaitForQueue(t, consumer)

	// Barrier: prove the subscription works before breaking the broker.
	if err := sender.Send(ctx, testEvent(service), []byte("before")); err != nil {
		t.Fatalf("send before restart: %v", err)
	}
	select {
	case <-delivered:
	case <-time.After(30 * time.Second):
		t.Fatal("the pre-restart event was never delivered, so the restart is not what this test measures")
	}

	// A restart long enough to outlast the ~1.6s retry budget.
	inst.StopApp(t)
	time.Sleep(5 * time.Second)
	inst.StartApp(t)

	select {
	case err := <-done:
		// The current, documented-by-this-test behavior.
		if err == nil {
			t.Fatal("Subscribe returned nil after a broker restart: a subscription that ended " +
				"must report why, or a caller cannot supervise it")
		}
		t.Logf("Subscribe returned after the broker restart, as expected today: %v", err)
	case <-time.After(60 * time.Second):
		// If we get here the library recovered on its own. That is BETTER than the
		// documented behavior, so prove the recovery is real rather than just
		// asserting the subscription is still running.
		if err := sender.Send(ctx, testEvent(service), []byte("after")); err != nil {
			t.Fatalf("send after restart: %v", err)
		}
		select {
		case <-delivered:
			t.Log("the subscription recovered by itself and delivered a post-restart event; " +
				"update this test's contract to require that")
		case <-time.After(30 * time.Second):
			t.Fatal("Subscribe neither returned nor delivered a post-restart event: the " +
				"subscription is wedged, consuming nothing and reporting nothing")
		}
	}

	stopSub()
}

// TestRequeueLoopIsBoundedByTheQuorumDeliveryLimit is the broker-backed proof that
// the default requeue policy behaves differently on the queue type most RabbitMQ 4.x
// estates actually run.
//
// TestTransientHandlerErrorDoesNotDestroyTheEvent proves a transient error does not
// destroy the event — on a CLASSIC queue, via a single redelivery. Quorum queues
// carry an x-delivery-limit (RabbitMQ 4.x applies one by default), and the receiver's
// requeueOnError default requeues on every transient failure, so the redelivery
// budget is finite: past the limit the broker dead-letters the message, or DROPS it
// when no dead-letter exchange is attached.
//
// The topology here is declared by the TEST, not by WithTopologySetup, which is the
// realistic case — a quorum queue provisioned by Terraform, an operator, or a policy.
// It also means this test is the one place the repo exercises a receiver consuming a
// queue it did not create.
func TestRequeueLoopIsBoundedByTheQuorumDeliveryLimit(t *testing.T) {
	inst := requireBroker(t)
	client := newTestClient(t)

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	const (
		service       = "quorum.v1"
		consumer      = "quorum-consumer"
		dlx           = "quorum-consumer.dlx"
		dlq           = "quorum-consumer.dead"
		deliveryLimit = 5
	)

	sd := &eventbus.ServiceDesc{ServiceName: service, Events: []eventbus.EventDesc{{Name: testEventName}}}

	sender := rabbitmq.NewSender(client)
	if err := sender.Setup(ctx, sd); err != nil {
		t.Fatalf("sender setup: %v", err)
	}

	// Externally-managed topology: a quorum queue with a finite delivery budget and
	// somewhere for the broker to put the message once it is spent.
	inst.DeclareExchange(t, dlx, "fanout")
	inst.DeclareQueue(t, dlq, nil)
	inst.BindQueue(t, dlq, "", dlx)
	inst.DeclareQueue(t, consumer, amqp.Table{
		"x-queue-type":           "quorum",
		"x-delivery-limit":       int32(deliveryLimit),
		"x-dead-letter-exchange": dlx,
		"x-dead-letter-strategy": "at-least-once",
		"x-overflow":             "reject-publish",
	})
	inst.BindQueue(t, consumer, testEventName, service)

	var (
		mu       sync.Mutex
		attempts int
	)
	spent := make(chan struct{})

	sub := eventbus.NewSubscriber(consumer)
	sub.RegisterHandler(sd, testEventName,
		func(context.Context, *event.Metadata, func(any) error, eventbus.SubscriberInterceptor) error {
			mu.Lock()
			attempts++
			n := attempts
			mu.Unlock()

			if n > deliveryLimit {
				select {
				case <-spent:
				default:
					close(spent)
				}
			}

			// Transient, as in the classic-queue test: retrying could succeed.
			return errors.New("downstream timeout")
		})

	// No WithTopologySetup: the queue above is not ours to declare.
	receiver := rabbitmq.NewReceiver(client)

	subCtx, stopSub := context.WithCancel(ctx)
	defer stopSub()
	done := make(chan error, 1)
	go func() { done <- sub.Subscribe(subCtx, receiver) }()

	if err := sender.Send(ctx, testEvent(service), []byte("payload")); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Wait for the budget to be spent, or for the loop to settle.
	select {
	case <-spent:
	case <-time.After(45 * time.Second):
	}

	// Let the broker finish dead-lettering before counting.
	time.Sleep(3 * time.Second)
	stopSub()
	<-done

	mu.Lock()
	got := attempts
	mu.Unlock()

	incoming, _ := inst.QueueDepth(t, consumer)
	dead, dlqExists := inst.QueueDepth(t, dlq)

	t.Logf("handler attempts=%d incoming depth=%d dead-letter depth=%d", got, incoming, dead)

	if got <= 1 {
		t.Fatalf("handler ran %d time(s): the message was not requeued at all", got)
	}
	if got > deliveryLimit+1 {
		t.Fatalf("handler ran %d times, more than the x-delivery-limit of %d allows: the broker's "+
			"redelivery budget is not bounding the requeue loop", got, deliveryLimit)
	}
	if !dlqExists {
		t.Fatal("the dead-letter queue disappeared")
	}
	// The event must be SOMEWHERE. incoming+dead == 0 is the silent-loss outcome a
	// quorum queue without a dead-letter exchange produces, and the reason pairing
	// the requeue default with a DLX is not optional.
	if incoming+dead == 0 {
		t.Fatalf("the event is in neither queue after %d delivery attempts: the delivery limit "+
			"destroyed it", got)
	}
	if dead != 1 {
		t.Errorf("dead-letter depth = %d, want 1: the exhausted message should be dead-lettered", dead)
	}

	// Read what the BROKER wrote, rather than a hand-built fixture: x-death is the
	// header the parking-lot receiver's retry-cap logic reads.
	if d, ok := inst.Get(t, dlq); ok {
		t.Logf("dead-lettered delivery: redelivered=%v x-death=%#v", d.Redelivered, d.Headers["x-death"])
	}
}

// TestTransientFailureOnAQuorumQueueSurvivesTheDeliveryLimit is the regression test for
// the requeue loop destroying events on the queue type production actually runs.
//
// The default receiver requeues on a transient handler error, and the broker redelivers
// IMMEDIATELY — there is no backoff anywhere in the path. On a classic queue that is
// merely wasteful. On a quorum queue, which carries an x-delivery-limit (RabbitMQ 4.x
// applies one by default), the loop spends the entire redelivery budget at broker speed:
// measured at 21 attempts in 13.9ms, after which the broker DISCARDS the message.
//
// So an event is destroyed by a downstream blip shorter than a network round trip — a
// connection reset, a 503, a context deadline — which is the most ordinary failure a
// consumer has, and exactly the case the requeue default was introduced to survive.
//
// The assertion is deliberately about TIME, not about attempt count. Retrying is correct;
// retrying a finite budget faster than the fault can clear is what loses the event. A
// transient fault that clears in a second or two must find the message still there.
func TestTransientFailureOnAQuorumQueueSurvivesTheDeliveryLimit(t *testing.T) {
	inst := requireBroker(t)
	client := newTestClient(t)

	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()

	const (
		service       = "qsurvive.v1"
		consumer      = "qsurvive-consumer"
		deliveryLimit = 20
		// How long the simulated downstream outage lasts. Well inside any sane retry
		// budget, and 100x the 13.9ms the unthrottled loop took to exhaust it.
		outage = 1500 * time.Millisecond
	)

	sd := &eventbus.ServiceDesc{ServiceName: service, Events: []eventbus.EventDesc{{Name: testEventName}}}

	sender := rabbitmq.NewSender(client)
	if err := sender.Setup(ctx, sd); err != nil {
		t.Fatalf("sender setup: %v", err)
	}

	// A quorum queue with the RabbitMQ 4.x default delivery limit and NO dead-letter
	// exchange: the plain setup, where an exhausted budget means the message is gone.
	inst.DeclareQueue(t, consumer, amqp.Table{
		"x-queue-type":     "quorum",
		"x-delivery-limit": int32(deliveryLimit),
	})
	inst.BindQueue(t, consumer, testEventName, service)

	var (
		mu        sync.Mutex
		attempts  int
		succeeded bool
	)
	recovered := make(chan struct{})
	start := time.Now()

	sub := eventbus.NewSubscriber(consumer)
	sub.RegisterHandler(sd, testEventName,
		func(context.Context, *event.Metadata, func(any) error, eventbus.SubscriberInterceptor) error {
			mu.Lock()
			attempts++
			mu.Unlock()

			// The downstream is down, then recovers.
			if time.Since(start) < outage {
				return errors.New("downstream blip")
			}

			mu.Lock()
			succeeded = true
			mu.Unlock()
			select {
			case <-recovered:
			default:
				close(recovered)
			}

			return nil
		})

	receiver := rabbitmq.NewReceiver(client)

	subCtx, stopSub := context.WithCancel(ctx)
	defer stopSub()
	done := make(chan error, 1)
	go func() { done <- sub.Subscribe(subCtx, receiver) }()

	if err := sender.Send(ctx, testEvent(service), []byte("payload")); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case <-recovered:
	case <-time.After(90 * time.Second):
	}

	stopSub()
	<-done

	mu.Lock()
	n, ok := attempts, succeeded
	mu.Unlock()
	depth, _ := inst.QueueDepth(t, consumer)

	t.Logf("attempts=%d handled_after_recovery=%v queue_depth=%d", n, ok, depth)

	if !ok {
		t.Fatalf("the event was never handled after the downstream recovered: %d delivery attempts "+
			"burned the quorum queue's %d-delivery budget before the %v outage cleared, and the "+
			"broker discarded it (queue depth %d). A transient failure must not destroy the event.",
			n, deliveryLimit, outage, depth)
	}
	if depth != 0 {
		t.Errorf("queue depth = %d after a successful handle, want 0", depth)
	}
}
