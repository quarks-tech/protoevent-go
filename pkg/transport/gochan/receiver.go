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

	for m := range r {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if err := processor(m.meta, m.data); err != nil {
				return err
			}
		}
	}

	return nil
}

func (r receiver) Setup(_ context.Context, _ string, _ ...eventbus.ServiceInfo) error {
	return nil
}
