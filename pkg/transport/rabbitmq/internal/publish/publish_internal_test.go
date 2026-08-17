package publish

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Both fakes must satisfy Channel, including NotifyReturn — the interface exists so a
// test can supply exactly the two behaviors no real broker can be asked for: a
// Confirm that blocks, and an already-closed NotifyReturn channel.

// blockingChannel is a Channel whose Confirm blocks until released, standing in
// for a channel whose confirm.select-ok never arrives (a blackholed TCP path).
// amqp091's Confirm is a synchronous RPC with no context and no timeout, so this is
// the real behavior, not a pessimistic invention.
type blockingChannel struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingChannel) Confirm(bool) error {
	b.once.Do(func() { close(b.entered) })
	<-b.release

	return nil
}

func (b *blockingChannel) IsClosed() bool { return false }

// fastChannel is an ordinary channel whose handshake completes immediately.
type fastChannel struct{ calls atomic.Int32 }

func (f *fastChannel) Confirm(bool) error {
	f.calls.Add(1)

	return nil
}

func (f *fastChannel) IsClosed() bool { return false }

// TestEnableConfirmsDoesNotSerializeDistinctChannels pins that one wedged
// confirm-mode handshake cannot stall publishes on unrelated channels.
//
// enableConfirms used to hold the sender's process-wide mutex across ch.Confirm, an
// unbounded context-less AMQP RPC. One channel whose confirm.select-ok never arrived
// therefore blocked EVERY concurrent Send — including ones whose own channel was
// already in confirm mode, and including the outbox relay's single Run goroutine.
// The bad case is correlated with the failure it should survive: new channels appear
// exactly during a network incident, because the pool re-dials, so the cliff arrived
// while the relay was trying to drain the backlog the outage created.
func TestEnableConfirmsDoesNotSerializeDistinctChannels(t *testing.T) {
	c := NewConfirms()

	wedged := &blockingChannel{entered: make(chan struct{}), release: make(chan struct{})}
	healthy := &fastChannel{}

	go func() { _ = c.Enable(wedged) }()

	// Only proceed once the wedged handshake is actually in flight, so the test
	// cannot pass by winning a race.
	select {
	case <-wedged.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the blocking handshake was never entered")
	}

	done := make(chan error, 1)
	go func() { done <- c.Enable(healthy) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("enableConfirms on a healthy channel = %v", err)
		}
	case <-time.After(5 * time.Second):
		close(wedged.release)
		t.Fatal("a healthy channel's confirm handshake was blocked by an unrelated wedged one: " +
			"every concurrent Send stalls behind a single unresponsive channel, including the " +
			"relay's Run goroutine")
	}

	close(wedged.release)
}

// TestEnableConfirmsRunsTheHandshakeOncePerChannel pins the bookkeeping the mutex
// was there for in the first place: confirm.select is one round trip per channel,
// not one per publish.
func TestEnableConfirmsRunsTheHandshakeOncePerChannel(t *testing.T) {
	c := NewConfirms()
	ch := &fastChannel{}

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if err := c.Enable(ch); err != nil {
				t.Errorf("enableConfirms: %v", err)
			}
		})
	}
	wg.Wait()

	if got := ch.calls.Load(); got != 1 {
		t.Fatalf("Confirm called %d times for one channel, want exactly 1", got)
	}
}

// TestReturnWatchSurvivesAClosedNotifyChannel pins that the unroutable-publish
// detector copes with the channel amqp091 hands it after a shutdown.
//
// amqp091 CLOSES every registered NotifyReturn channel when the AMQP channel shuts
// down (channel.go: `for _, c := range ch.returns { close(c) }`), and NotifyReturn on
// an already-shut-down channel returns an immediately-closed channel via the noNotify
// branch. A receive from a closed channel is always ready, which breaks both halves of
// returnWatch:
//
//   - drain() loops `select { case <-w.ch: ; default: return }`, so on a closed channel
//     it never reaches default: an infinite busy-loop at 100% CPU, holding watch.mu.
//     In the Sender that permanently stalls the relay's single Run goroutine — with no
//     Drained, no Error, no log, and lane.Stuck unreachable because Send never returns
//     — and it bypasses lane.SendTimeout entirely, because drain() consults no context.
//   - took() reports (zeroReturn, true), i.e. a FALSE unroutable. That fires on the
//     most common ordering: a channel exception nacks the outstanding confirm, and the
//     accurate "channel closed before the confirm arrived" error is replaced by
//     ErrUnroutable with a giveaway reply code of 0. An UnsendableClassifier keyed on
//     ErrUnroutable — which relay.UnsendableClassifier's own doc invites — would then
//     classify a TRANSIENT broker restart as permanent and bulk-divert the backlog to
//     the DLQ.
//
// Reachable both on a map hit whose cached channel has since died, and on a map miss
// against an already-closed channel.
func TestReturnWatchSurvivesAClosedNotifyChannel(t *testing.T) {
	t.Run("drain returns", func(t *testing.T) {
		w := &Watch{ch: make(chan amqp.Return, 1)}
		close(w.ch)

		done := make(chan struct{})
		go func() {
			w.drain()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("drain() did not return on a closed NotifyReturn channel: it spins at 100% CPU " +
				"holding watch.mu, permanently stalling the relay's Run goroutine and bypassing " +
				"lane.SendTimeout")
		}
	})

	t.Run("took reports no return", func(t *testing.T) {
		w := &Watch{ch: make(chan amqp.Return, 1)}
		close(w.ch)

		if ret, ok := w.Took(); ok {
			t.Fatalf("took() = (%+v, true) on a closed NotifyReturn channel, want (_, false): a "+
				"closed channel is not a basic.return, and reporting one turns a transient "+
				"channel closure into a permanent ErrUnroutable", ret)
		}
	})
}

// NotifyReturn satisfies Channel. Returns an open channel: the closed-channel case is
// exercised directly against Watch, which is where that hazard lives.
func (b *blockingChannel) NotifyReturn(c chan amqp.Return) chan amqp.Return { return c }

// NotifyReturn satisfies Channel.
func (f *fastChannel) NotifyReturn(c chan amqp.Return) chan amqp.Return { return c }
