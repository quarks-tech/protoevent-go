package gochan

import (
	"context"

	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
)

type receiver <-chan message

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
		case m, ok := <-r:
			if !ok {
				return nil
			}
			// Deliberately NOT re-checking ctx here. Cancellation can still win the
			// race above and land between the dequeue and this point; dropping the
			// message then would lose it outright, which is strictly worse for an
			// at-least-once bus than running one extra handler call after a
			// cancellation the pre-check will catch on the next iteration.
			if err := processor(m.meta, m.data); err != nil {
				return err
			}
		}
	}
}

func (r receiver) Setup(_ context.Context, _ string, _ ...eventbus.ServiceInfo) error {
	return nil
}
