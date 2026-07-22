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

	// Select on ctx and the channel together: ranging over the channel alone
	// would observe cancellation only when a message arrives, leaving an idle
	// subscriber blocked until Close despite a canceled ctx.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m, ok := <-r:
			if !ok {
				return nil
			}
			if err := processor(m.meta, m.data); err != nil {
				return err
			}
		}
	}
}

func (r receiver) Setup(_ context.Context, _ string, _ ...eventbus.ServiceInfo) error {
	return nil
}
