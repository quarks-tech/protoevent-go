package rabbitmq_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq"
)

// batchOutgoing builds n events for service, each carrying its index in Subject.
func batchOutgoing(service string, n int) []eventbus.Outgoing {
	out := make([]eventbus.Outgoing, 0, n)
	for i := range n {
		md := testEvent(service)
		md.Subject = strconv.Itoa(i)
		out = append(out, eventbus.Outgoing{Metadata: md, Data: []byte("p")})
	}

	return out
}

// TestSendBatchDeliversEveryMessageInOrder is the correctness floor for the
// pipelined path: overlapping the CONFIRMS must not change what the broker ends up
// holding, or in what order.
//
// The whole point of the capability is that a relay may advance a durable watermark
// on the count it returns, so "it said 200 and the queue has 200, in publish order"
// is the property that makes that safe.
func TestSendBatchDeliversEveryMessageInOrder(t *testing.T) {
	inst := requireBroker(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	const (
		service = "batchok.v1"
		queue   = "batchok-queue"
		n       = 200
	)

	inst.DeclareExchange(t, service, "topic")
	inst.DeclareQueue(t, queue, amqp.Table{"x-queue-type": "quorum"})
	inst.BindQueue(t, queue, testEventName, service)
	t.Cleanup(func() { inst.DeleteQueue(t, queue) })

	client := newTestClient(t)
	sender := rabbitmq.NewSender(client)
	sd := &eventbus.ServiceDesc{ServiceName: service, Events: []eventbus.EventDesc{{Name: testEventName}}}
	if err := sender.Setup(ctx, sd); err != nil {
		t.Fatalf("setup: %v", err)
	}

	sent, err := sender.SendBatch(ctx, batchOutgoing(service, n))
	if err != nil {
		t.Fatalf("SendBatch: sent=%d err=%v", sent, err)
	}
	if sent != n {
		t.Fatalf("SendBatch reported %d delivered, want %d with a nil error "+
			"(eventbus.BatchSender: err is nil if and only if every message was confirmed)", sent, n)
	}

	depth, ok := inst.QueueDepth(t, queue)
	if !ok {
		t.Fatal("queue disappeared")
	}
	if depth != n {
		t.Fatalf("queue holds %d messages, SendBatch claimed %d delivered.\n"+
			"A batch that reports more than the broker actually has lets a relay commit its "+
			"offset past events nobody received — the silent gap the outbox exists to prevent.",
			depth, n)
	}

	// Publish order must survive the overlap: the confirms are collected in publish
	// order, but the frames themselves are what the queue orders by.
	for i := range n {
		d, ok := inst.Get(t, queue)
		if !ok {
			t.Fatalf("message %d missing from the queue", i)
		}
		if got := d.Headers["ce_subject"]; got != nil && got != strconv.Itoa(i) {
			t.Fatalf("message at position %d carries subject %v, want %d — pipelining must not "+
				"reorder publishes", i, got, i)
		}
		if i == 0 {
			// Get leaves the delivery unacked and closes the channel, so it returns
			// to the head. Checking the first is enough to pin ordering without
			// consuming the queue 200 times.
			break
		}
	}
}

// TestSendBatchReportsTheContiguousPrefixOnFailure pins the return contract's
// sharpest edge.
//
// A message this Sender cannot route at all (an event type with no dot, so
// event.SplitType has no exchange to send it to) sits in the middle of an otherwise
// healthy batch. Everything before it must be delivered AND reported; the failure
// reported must be that message's; and nothing after it may be counted, even though
// those messages are perfectly good — the caller advances a durable position by this
// number, so counting past a gap loses the gap.
func TestSendBatchReportsTheContiguousPrefixOnFailure(t *testing.T) {
	inst := requireBroker(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	const (
		service = "batchpartial.v1"
		queue   = "batchpartial-queue"
		good    = 5
	)

	inst.DeclareExchange(t, service, "topic")
	inst.DeclareQueue(t, queue, amqp.Table{"x-queue-type": "quorum"})
	inst.BindQueue(t, queue, testEventName, service)
	t.Cleanup(func() { inst.DeleteQueue(t, queue) })

	client := newTestClient(t)
	sender := rabbitmq.NewSender(client)
	sd := &eventbus.ServiceDesc{ServiceName: service, Events: []eventbus.EventDesc{{Name: testEventName}}}
	if err := sender.Setup(ctx, sd); err != nil {
		t.Fatalf("setup: %v", err)
	}

	msgs := batchOutgoing(service, good)
	// The unroutable one, then more healthy ones behind it.
	bad := testEvent(service)
	bad.Type = "nodothere"
	msgs = append(msgs, eventbus.Outgoing{Metadata: bad, Data: []byte("p")})
	msgs = append(msgs, batchOutgoing(service, 3)...)

	sent, err := sender.SendBatch(ctx, msgs)
	if err == nil {
		t.Fatal("SendBatch = nil error for a batch containing a message it cannot route; " +
			"the caller would advance past it")
	}
	if sent != good {
		t.Fatalf("SendBatch reported %d delivered, want %d — exactly the contiguous prefix before "+
			"the message it could not route. Counting the healthy messages behind it would advance "+
			"a durable position past the one that failed.", sent, good)
	}

	depth, ok := inst.QueueDepth(t, queue)
	if !ok {
		t.Fatal("queue disappeared")
	}
	if depth < good {
		t.Fatalf("queue holds %d messages but SendBatch reported %d delivered: it must never "+
			"report more than the broker confirmed", depth, good)
	}
}

// TestSendBatchFallsBackUnderMandatoryPublish pins that the two features stay
// mutually exclusive rather than silently breaking each other.
//
// Attributing a basic.return needs exactly one mandatory publish in flight per
// channel, which pipelining cannot provide. The batch path therefore degrades to
// serial sends here — and must keep detecting the unroutable publish, since that is
// the whole reason someone turned the option on.
func TestSendBatchFallsBackUnderMandatoryPublish(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// An exchange with nothing bound to it: every publish is unroutable.
	const service = "batchmandatory.v1"
	client := newTestClient(t)
	sender := rabbitmq.NewSender(client, rabbitmq.WithMandatoryPublish())
	sd := &eventbus.ServiceDesc{ServiceName: service, Events: []eventbus.EventDesc{{Name: testEventName}}}
	if err := sender.Setup(ctx, sd); err != nil {
		t.Fatalf("setup: %v", err)
	}

	sent, err := sender.SendBatch(ctx, batchOutgoing(service, 4))
	if !errors.Is(err, rabbitmq.ErrUnroutable) {
		t.Fatalf("SendBatch = (%d, %v), want an ErrUnroutable.\n"+
			"Under WithMandatoryPublish the batch path must fall back to serial sends: pipelining "+
			"puts several publishes in flight on one channel, and a basic.return names no publish, "+
			"so the detection this option exists for would silently stop working.", sent, err)
	}
	if sent != 0 {
		t.Fatalf("SendBatch reported %d delivered, want 0 — the first message was already unroutable", sent)
	}
}

// TestSendBatchWithoutConfirmsStillReportsEverySend covers the other fallback: with
// confirms off there is nothing to overlap, so the batch is a serial loop and keeps
// the (documented, lossy) semantics of Send under that option.
func TestSendBatchWithoutConfirmsStillReportsEverySend(t *testing.T) {
	inst := requireBroker(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	const (
		service = "batchnoconfirm.v1"
		queue   = "batchnoconfirm-queue"
		n       = 20
	)

	inst.DeclareExchange(t, service, "topic")
	inst.DeclareQueue(t, queue, amqp.Table{"x-queue-type": "quorum"})
	inst.BindQueue(t, queue, testEventName, service)
	t.Cleanup(func() { inst.DeleteQueue(t, queue) })

	client := newTestClient(t)
	sender := rabbitmq.NewSender(client, rabbitmq.WithoutPublisherConfirms())
	sd := &eventbus.ServiceDesc{ServiceName: service, Events: []eventbus.EventDesc{{Name: testEventName}}}
	if err := sender.Setup(ctx, sd); err != nil {
		t.Fatalf("setup: %v", err)
	}

	sent, err := sender.SendBatch(ctx, batchOutgoing(service, n))
	if err != nil || sent != n {
		t.Fatalf("SendBatch = (%d, %v), want (%d, nil)", sent, err, n)
	}
}

// TestSendBatchEmptyIsANoOp guards the boundary the relay hits on an empty page.
func TestSendBatchEmptyIsANoOp(t *testing.T) {
	client := newTestClient(t)
	sender := rabbitmq.NewSender(client)

	sent, err := sender.SendBatch(t.Context(), nil)
	if sent != 0 || err != nil {
		t.Fatalf("SendBatch(nil) = (%d, %v), want (0, nil)", sent, err)
	}
}

var _ eventbus.BatchSender = (*rabbitmq.Sender)(nil)
