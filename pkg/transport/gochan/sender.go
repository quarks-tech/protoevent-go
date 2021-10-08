package gochan

import (
	"context"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

// Sender implements Sender by sending Messages on a channel.
type sender chan<- message

func (s sender) Send(ctx context.Context, meta *event.Metadata, data []byte) error {
	if ctx == nil {
		return ErrNilContext
	} else if meta == nil {
		return ErrNilMetadata
	}

	m := message{
		meta: meta,
		data: data,
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case s <- m:
		return nil
	}
}
