package gochan

import (
	"context"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
)

type receiver struct {
	ch    <-chan message
	onErr func(md *event.Metadata, err error)
}

func (r receiver) Receive(ctx context.Context, processor eventbus.Processor) error {
	if ctx == nil {
		return ErrNilContext
	}

	for {
		// Cancellation has priority, and it is checked BEFORE dequeuing: Go picks
		// a ready case pseudo-randomly, so the blocking select below would keep
		// winning the channel case on an already-canceled ctx and drain the whole
		// buffer through the processor. Checking here also means a canceled
		// receiver never takes a message it will not process — a message dequeued
		// and then dropped is lost, since there is no way to put it back.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Select on ctx and the channel together: ranging over the channel alone
		// would observe cancellation only when a message arrives, leaving an idle
		// subscriber blocked until Close despite a canceled ctx.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m, ok := <-r.ch:
			if !ok {
				return nil
			}
			// Deliberately NOT re-checking ctx here. Cancellation can still win the
			// race above and land between the dequeue and this point; dropping the
			// message then would lose it outright, which is strictly worse for an
			// at-least-once bus than running one extra handler call after a
			// cancellation the pre-check will catch on the next iteration.
			//
			// A handler failure does NOT end the subscription. Returning the error —
			// which this used to do — killed the whole subscriber goroutine on the
			// first unhandled event: every message still buffered, and everything
			// published afterwards, was silently never processed, while Send kept
			// succeeding until the buffer filled and publishers blocked forever. One
			// bad event is not a broken transport, and the documented contract is to
			// deliver until the channel closes or ctx is canceled.
			//
			// There is nowhere to put the failed message back — this transport has no
			// redelivery and no dead-letter queue — so it is dropped, and reported to
			// the error handler if one was installed (see WithErrorHandler).
			if err := processor(ctx, m.meta, m.data); err != nil && r.onErr != nil {
				r.onErr(m.meta, err)
			}
		}
	}
}

func (r receiver) Setup(_ context.Context, _ string, _ ...eventbus.ServiceInfo) error {
	return nil
}
