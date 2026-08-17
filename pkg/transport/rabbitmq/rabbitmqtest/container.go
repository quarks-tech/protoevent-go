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

	// raw is the lazily dialed connection behind the queue helpers below.
	once   sync.Once
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
	inst := &Instance{Address: endpoint, URL: "amqp://" + endpoint}
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

// conn returns the shared raw connection, dialing it once.
func (i *Instance) conn(t *testing.T) *amqp.Connection {
	t.Helper()

	i.once.Do(func() {
		i.raw, i.rawErr = amqp.Dial(i.URL)
	})
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
