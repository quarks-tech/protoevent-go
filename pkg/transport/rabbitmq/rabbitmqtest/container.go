// Package rabbitmqtest boots an ephemeral RabbitMQ (testcontainers) for
// integration tests.
//
// It exists because the per-delivery policies in this module — what happens to a
// message whose handler returned an error, whether an unroutable publish is
// detected, whether a park is durable before the original is acked — are decided by
// the BROKER, not by this code. Tests against a fake command processor can only
// assert which arguments were passed; they cannot show that a Reject(false) on a
// queue with no dead-letter exchange destroys the message, which is the behavior
// that matters.
package rabbitmqtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	rabbitImage    = "rabbitmq:4-alpine"
	startupTimeout = 180 * time.Second
	amqpPort       = "5672/tcp"
)

// ErrDockerUnavailable marks errors caused by Docker/the container runtime being
// unavailable, as opposed to a genuine harness bug. Callers should use errors.Is
// against this sentinel to decide whether a Start failure is an acceptable skip or
// a real failure. Mirrors tidbtest/mongodbtest.
var ErrDockerUnavailable = errors.New("docker unavailable")

// Instance is a ready broker.
type Instance struct {
	// Address is the amqpx.Config.Address form — credentials, host:port and vhost
	// with NO amqp:// scheme, which is what amqpx expects.
	Address string
	// URL is the same endpoint as a full amqp:// DSN, for dialing amqp091 directly
	// (a test that inspects the broker without going through this module).
	URL string

	// container is the running broker, kept so a test can reach the broker as an
	// OPERATOR rather than as a client: rabbitmqctl for the states that have no AMQP
	// representation — a memory or disk alarm, a stopped app, a changed
	// max_message_size. Start used to discard this handle, which made every such
	// failure mode structurally untestable: a broker restart, a blocked connection,
	// and a quorum queue's delivery limit all need it.
	container testcontainers.Container

	// raw is the lazily dialed connection behind the queue helpers below. Guarded by
	// mu rather than a sync.Once because it is re-dialed after a broker restart.
	mu     sync.Mutex
	raw    *amqp.Connection
	rawErr error
}

// Start boots RabbitMQ and returns a ready Instance + cleanup. Returns an error
// wrapping ErrDockerUnavailable when Docker is unavailable; any other error is a
// genuine harness bug and callers should fail loudly on it.
func Start(ctx context.Context) (*Instance, func(), error) {
	// Probe the daemon explicitly BEFORE starting a container, so only a
	// genuinely-unavailable daemon maps to ErrDockerUnavailable; any error from the
	// container start below (with a healthy daemon) is a real harness bug.
	if err := probeDocker(ctx); err != nil {
		return nil, nil, fmt.Errorf("probe docker daemon: %w", errors.Join(ErrDockerUnavailable, err))
	}

	req := testcontainers.ContainerRequest{
		Image:        rabbitImage,
		ExposedPorts: []string{amqpPort},
		// The log line is the only reliable readiness signal: the AMQP port accepts
		// TCP before the broker will complete a connection handshake, so waiting on
		// the port alone yields flaky "channel/connection is not open" failures at
		// the start of a run.
		WaitingFor: wait.ForLog("Server startup complete").WithStartupTimeout(startupTimeout),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req, Started: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("start rabbitmq container: %w", err)
	}

	// context.Background, not ctx: cleanup runs when the caller is tearing down, by
	// which point its context is typically already canceled — and a canceled context
	// would skip the container teardown and leak it.
	cleanup := func() { _ = c.Terminate(context.Background()) } //nolint:contextcheck // see above
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()

	host, err := c.Host(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get rabbitmq container host: %w", err)
	}
	mapped, err := c.MappedPort(ctx, amqpPort)
	if err != nil {
		return nil, nil, fmt.Errorf("get rabbitmq mapped port: %w", err)
	}

	endpoint := fmt.Sprintf("guest:guest@%s:%s/", host, mapped.Port())
	inst := &Instance{Address: endpoint, URL: "amqp://" + endpoint, container: c}
	terminate := cleanup
	cleanup = nil // disarm the deferred unwind: ownership passes to the caller

	return inst, terminate, nil
}

// probeDocker reports whether the Docker daemon is reachable, via the
// testcontainers Docker provider's own health check. The provider is closed by
// Health itself.
func probeDocker(ctx context.Context) error {
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		return fmt.Errorf("create docker provider: %w", err)
	}
	if err := provider.Health(ctx); err != nil {
		return fmt.Errorf("docker daemon health check: %w", err)
	}

	return nil
}

// Queue helpers for tests that need to inspect the broker WITHOUT going through the
// module under test.
//
// They live here, on Instance, because both the rabbitmq module's own broker suite and
// the never-published test/e2e module needed the same four operations and had grown
// separate copies with already-divergent timeouts. test/e2e imports this package
// already.
//
// All of them go through one long-lived raw connection, opening a fresh CHANNEL per
// call. That matters for two reasons. A passive declare of a missing queue is a 404
// channel exception, so each call must be isolated to its own channel — but only the
// channel, not the connection; the earlier copies re-dialed (TCP + AMQP handshake,
// ~7 round trips) on every 200ms poll iteration. And the connection is deliberately
// NOT amqpx's pooled one, so a 404 cannot retire a channel the code under test is
// about to use.

// conn returns the shared raw connection, dialing it on first use and RE-dialing
// after the broker has dropped it.
//
// The re-dial is what makes StopApp/StartApp usable: stopping the app kills every
// connection, so a cached one is permanently closed and every later helper call
// would fail on a broker that is in fact healthy again.
func (i *Instance) conn(t *testing.T) *amqp.Connection {
	t.Helper()

	i.mu.Lock()
	defer i.mu.Unlock()

	if i.raw == nil || i.raw.IsClosed() {
		i.raw, i.rawErr = amqp.Dial(i.URL)
	}
	if i.rawErr != nil {
		t.Fatalf("dial broker: %v", i.rawErr)
	}

	return i.raw
}

// withChannel runs fn on a fresh channel, so a channel exception cannot affect the
// caller's next call.
func (i *Instance) withChannel(t *testing.T, fn func(*amqp.Channel) error) error {
	t.Helper()

	ch, err := i.conn(t).Channel()
	if err != nil {
		t.Fatalf("open channel: %v", err)
	}
	defer func() { _ = ch.Close() }()

	return fn(ch)
}

// QueueDepth reports how many messages queue holds, and whether it exists at all. A
// missing queue is (0, false) rather than a fatal: "the message is nowhere" is a
// legitimate thing for a test to assert.
func (i *Instance) QueueDepth(t *testing.T, queue string) (int, bool) {
	t.Helper()

	var depth int
	exists := i.withChannel(t, func(ch *amqp.Channel) error {
		q, err := ch.QueueDeclarePassive(queue, true, false, false, false, nil)
		if err != nil {
			return err
		}
		depth = q.Messages

		return nil
	}) == nil

	return depth, exists
}

// QueueExists reports whether queue has been declared.
func (i *Instance) QueueExists(t *testing.T, queue string) bool {
	t.Helper()
	_, ok := i.QueueDepth(t, queue)

	return ok
}

// WaitForQueue blocks until queue exists, so a publish cannot race topology setup and
// be dropped as unroutable — which is silent by default.
func (i *Instance) WaitForQueue(t *testing.T, queue string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if i.QueueExists(t, queue) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("queue %q was not declared within the timeout", queue)
}

// DeleteQueue removes queue, for tests that simulate operator-driven topology drift.
func (i *Instance) DeleteQueue(t *testing.T, queue string) {
	t.Helper()

	if err := i.withChannel(t, func(ch *amqp.Channel) error {
		_, err := ch.QueueDelete(queue, false, false, false)

		return err
	}); err != nil {
		t.Fatalf("delete queue %q: %v", queue, err)
	}
}

// Operator helpers: states the broker can be in that have no AMQP client-side
// representation.
//
// A client can declare, publish and consume; it cannot put the broker into a
// resource alarm, stop its app, or change max_message_size. Those are the
// preconditions for a whole class of production failures — a connection blocked by a
// disk alarm, a subscription that never recovers from a rolling broker restart, an
// oversize publish rejected by policy — so a test that cannot reach them cannot
// cover them.

// Container exposes the running broker for a test that needs an operation these
// helpers do not wrap.
func (i *Instance) Container() testcontainers.Container {
	return i.container
}

// Exec runs cmd inside the broker container and returns its exit code and combined
// output. It fails the test only when the command could not be run at all; a NON-ZERO
// exit is returned for the caller to assert on, since "rabbitmqctl refused this" is
// often the thing under test.
func (i *Instance) Exec(t *testing.T, cmd ...string) (int, string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	code, reader, err := i.container.Exec(ctx, cmd)
	if err != nil {
		t.Fatalf("exec %v: %v", cmd, err)
	}

	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read output of %v: %v", cmd, err)
	}

	return code, string(out)
}

// Rabbitmqctl runs `rabbitmqctl <args...>` and fails the test on a non-zero exit,
// for the cases where the command succeeding is a precondition rather than the
// assertion.
func (i *Instance) Rabbitmqctl(t *testing.T, args ...string) string {
	t.Helper()

	code, out := i.Exec(t, append([]string{"rabbitmqctl"}, args...)...)
	if code != 0 {
		t.Fatalf("rabbitmqctl %v exited %d: %s", args, code, out)
	}

	return out
}

// StopApp stops the RabbitMQ application while leaving the container running, and
// StartApp brings it back. Together they model a broker restart: every connection
// and channel is dropped, and the endpoint is unchanged when it returns.
//
// This is deliberately NOT a container stop/start. Docker republishes an ephemeral
// host port on restart, so the container-level cycle would move the address every
// caller already captured — the app-level cycle drops connections just as hard while
// keeping Address and URL valid.
func (i *Instance) StopApp(t *testing.T) {
	t.Helper()
	i.Rabbitmqctl(t, "stop_app")
}

// StartApp restarts the application stopped by StopApp and waits for it to accept
// connections again, so a test does not have to poll for readiness itself.
func (i *Instance) StartApp(t *testing.T) {
	t.Helper()

	i.Rabbitmqctl(t, "start_app")
	i.Rabbitmqctl(t, "await_startup")

	// await_startup returns once the node is up; prove the AMQP listener actually
	// completes a handshake before handing control back, which is the same reason
	// Start waits on the log line rather than on the port.
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, err := amqp.Dial(i.URL)
		if err == nil {
			_ = c.Close()

			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("broker did not accept connections after start_app: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// SetMemoryWatermark sets vm_memory_high_watermark. Passing 0 raises the memory
// alarm immediately, which blocks every publishing connection — the state that makes
// a context-less publish hang. Restore it with a normal value (0.6 is the default).
func (i *Instance) SetMemoryWatermark(t *testing.T, fraction string) {
	t.Helper()
	i.Rabbitmqctl(t, "set_vm_memory_high_watermark", fraction)
}

// SetDiskFreeLimit sets disk_free_limit. A limit larger than the disk (e.g. "100GB")
// raises the disk alarm, blocking publishers.
func (i *Instance) SetDiskFreeLimit(t *testing.T, limit string) {
	t.Helper()
	i.Rabbitmqctl(t, "set_disk_free_limit", limit)
}

// SetMaxMessageSize changes the broker's max_message_size at runtime, so a test can
// provoke a permanently-rejected publish without building a multi-megabyte payload.
func (i *Instance) SetMaxMessageSize(t *testing.T, bytes int) {
	t.Helper()
	i.Rabbitmqctl(t, "eval", fmt.Sprintf("application:set_env(rabbit, max_message_size, %d).", bytes))
}

// DeclareQueue declares a durable queue with args, for the topology a test needs the
// BROKER to own rather than the code under test — a quorum queue, an
// x-delivery-limit, a dead-letter exchange, a message TTL. Declaring these here is
// what makes "the receiver did not create this topology" a testable condition.
func (i *Instance) DeclareQueue(t *testing.T, queue string, args amqp.Table) {
	t.Helper()

	if err := i.withChannel(t, func(ch *amqp.Channel) error {
		_, err := ch.QueueDeclare(queue, true, false, false, false, args)

		return err
	}); err != nil {
		t.Fatalf("declare queue %q with args %v: %v", queue, args, err)
	}
}

// DeclareExchange declares a durable exchange of kind, for the same reason as
// DeclareQueue.
func (i *Instance) DeclareExchange(t *testing.T, exchange, kind string) {
	t.Helper()

	if err := i.withChannel(t, func(ch *amqp.Channel) error {
		return ch.ExchangeDeclare(exchange, kind, true, false, false, false, nil)
	}); err != nil {
		t.Fatalf("declare exchange %q: %v", exchange, err)
	}
}

// BindQueue binds queue to exchange under key.
func (i *Instance) BindQueue(t *testing.T, queue, key, exchange string) {
	t.Helper()

	if err := i.withChannel(t, func(ch *amqp.Channel) error {
		return ch.QueueBind(queue, key, exchange, false, nil)
	}); err != nil {
		t.Fatalf("bind queue %q to %q with key %q: %v", queue, exchange, key, err)
	}
}

// Get fetches one message from queue without acking it, and reports whether there was
// one. It is how a test reads what the BROKER wrote — most importantly the real
// x-death header shape behind a retry lap, which is otherwise asserted only against
// hand-built fixtures.
//
// The delivery is left unacked and the channel is closed immediately after, so the
// message returns to the queue: inspecting a queue must not consume it.
func (i *Instance) Get(t *testing.T, queue string) (amqp.Delivery, bool) {
	t.Helper()

	var (
		d  amqp.Delivery
		ok bool
	)
	if err := i.withChannel(t, func(ch *amqp.Channel) error {
		var err error
		d, ok, err = ch.Get(queue, false)

		return err
	}); err != nil {
		t.Fatalf("get from queue %q: %v", queue, err)
	}

	return d, ok
}
