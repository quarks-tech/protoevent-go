// Package publish holds the per-channel publisher-confirm bookkeeping shared by
// everything in this module that publishes: rabbitmq.Sender's ordinary publishes and
// parkinglot.Receiver's park publishes.
//
// It exists for the same reason internal/consume does, and the evidence is sharper
// here. Both publishers used to carry byte-identical copies of the confirm-mode
// handshake and the basic.return watch, and the copies had ALREADY diverged in one
// commit: the handshake was reworked in the Sender to run the AMQP RPC outside the
// mutex, and its acknowledged mirror in the parking lot kept holding the mutex across
// it — where it was worse than the original, because that same mutex had since been
// given a second job. A closed-channel busy-loop then had to be fixed twice, and only
// one copy ended up with a regression test.
//
// The two hazards this package encapsulates are both non-obvious, which is exactly
// why they should be written down once:
//
//   - Channel.Confirm is a synchronous, context-less AMQP RPC. Holding a
//     process-wide mutex across it lets one unresponsive channel stall every
//     concurrent publish, and new channels appear precisely during the network
//     incident that makes a channel unresponsive.
//   - amqp091 CLOSES a registered NotifyReturn channel when its AMQP channel shuts
//     down, and returns an already-closed channel if you register after that. A
//     receive from a closed channel is always ready, so naive draining spins and a
//     naive read reports a phantom basic.return.
package publish

import (
	"fmt"
	"sync"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Channel is the part of *amqp.Channel this package needs.
//
// It is an interface so a test can supply a Confirm that blocks or a NotifyReturn
// that is already closed — the two failures this bookkeeping exists to survive, and
// neither of which a real broker can be asked to produce on demand.
type Channel interface {
	Confirm(noWait bool) error
	IsClosed() bool
	NotifyReturn(c chan amqp.Return) chan amqp.Return
}

// Confirms tracks, per channel, whether confirm mode has been negotiated and (when
// mandatory publishing is on) that channel's basic.return watch.
//
// One map rather than two: both are keyed by the same channel with the same lifetime
// and the same eviction rule, and keeping them apart meant two sweeps to keep in step
// and a channel that could be evicted from one while surviving in the other.
//
// Safe for concurrent use. The mutex guards the MAP ONLY, never an AMQP round trip.
type Confirms struct {
	mu    sync.Mutex
	state map[Channel]*channelState
}

type channelState struct {
	// once serializes the confirm.select handshake per channel without holding the
	// map lock across it, so a wedged channel blocks only publishes that need it.
	once sync.Once
	err  error

	// returns is created on first use, and only under mandatory publishing.
	returnsOnce sync.Once
	returns     *Watch
}

// NewConfirms returns an empty tracker.
func NewConfirms() *Confirms {
	return &Confirms{state: make(map[Channel]*channelState)}
}

// stateFor returns ch's bookkeeping, creating it if absent.
//
// Channels the pool has retired are forgotten on the miss path only — that is once
// per new channel, i.e. exactly as often as the map grows, so its size settles at the
// live pool size instead of accumulating for the process lifetime.
func (c *Confirms) stateFor(ch Channel) *channelState {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state == nil {
		c.state = make(map[Channel]*channelState)
	}
	if st, ok := c.state[ch]; ok {
		return st
	}
	for existing := range c.state {
		if existing.IsClosed() {
			delete(c.state, existing)
		}
	}
	st := &channelState{}
	c.state[ch] = st

	return st
}

// Enable puts ch into publisher-confirm mode, at most once per channel.
//
// The handshake runs OUTSIDE the map lock, which is the whole point of the sync.Once
// indirection: ch.Confirm is an unbounded context-less RPC, and holding a shared
// mutex across it stalls every concurrent publish — including ones on healthy
// channels — at exactly the moment a network incident is creating new channels.
func (c *Confirms) Enable(ch Channel) error {
	st := c.stateFor(ch)

	st.once.Do(func() {
		if err := ch.Confirm(false); err != nil {
			st.err = fmt.Errorf("enable publisher confirms: %w", err)
		}
	})

	return st.err
}

// Returns registers (once per channel) and returns ch's basic.return watch, for
// detecting a publish the exchange routed to no queue.
func (c *Confirms) Returns(ch Channel) *Watch {
	st := c.stateFor(ch)

	st.returnsOnce.Do(func() {
		// Buffered generously: amqp091 sends on this channel from its single reader
		// goroutine, and a blocked send there would stall every delivery on the
		// connection.
		st.returns = &Watch{ch: ch.NotifyReturn(make(chan amqp.Return, 16))}
	})

	return st.returns
}

// Watch is one channel's unroutable-publish detector.
//
// A basic.return carries no publish identifier, so attributing one requires knowing
// which publish was in flight. RabbitMQ emits basic.return BEFORE the basic.ack for
// the same publish and amqp091 dispatches both from its single reader goroutine in
// frame order, so a return present once our ack has arrived is ours — provided only
// one mandatory publish is in flight on this channel at a time, which is what Lock
// enforces.
//
// That serialization is why mandatory publishing is opt-in rather than free: pooled
// channels are shared, so concurrent publishers on one channel take turns. A relay
// publishes serially from its single Run goroutine and pays nothing.
type Watch struct {
	mu sync.Mutex
	ch chan amqp.Return
}

// Lock claims the channel for one publish/confirm pair and discards any return left
// over from an earlier publish, so a stale one cannot be misread as this publish's.
// The returned function releases it.
func (w *Watch) Lock() func() {
	w.mu.Lock()
	w.drain()

	return w.mu.Unlock
}

// drain discards pending returns.
func (w *Watch) drain() {
	for {
		select {
		case _, ok := <-w.ch:
			// A CLOSED channel is always ready to receive, so a single-value receive
			// here would never reach the default branch: it becomes an infinite
			// busy-loop holding w.mu. amqp091 closes every registered NotifyReturn
			// channel when the AMQP channel shuts down, and hands back an
			// already-closed channel if NotifyReturn is called after that — so this is
			// the ordinary outcome of a connection drop or a channel exception, not an
			// edge case.
			if !ok {
				return
			}
		default:
			return
		}
	}
}

// Took reports the return for the publish just confirmed, if there was one.
func (w *Watch) Took() (amqp.Return, bool) {
	select {
	case ret, ok := <-w.ch:
		// A closed channel is not a basic.return. Reporting one would turn a
		// TRANSIENT channel closure — the ordinary result of a channel exception
		// nacking the outstanding confirm — into a permanent unroutable verdict,
		// masking the accurate "channel closed before the confirm arrived" error and
		// inviting an UnsendableClassifier to bulk-divert the backlog to the DLQ.
		return ret, ok
	default:
		return amqp.Return{}, false
	}
}

// AssumeEnabled marks ch as already in confirm mode WITHOUT performing the handshake.
//
// For this module's own tests only. A test that drives a publish path against a
// zero-value *amqp.Channel cannot let Enable run, because Confirm panics on one — and
// then the panic under test would come from the handshake rather than from the publish,
// making the assertion pass for the wrong reason. This is exported (within the internal
// package) rather than reached through a struct literal so the map's invariants stay
// owned here.
func (c *Confirms) AssumeEnabled(ch Channel) {
	st := c.stateFor(ch)
	st.once.Do(func() {})
}
