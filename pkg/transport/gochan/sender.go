package gochan

import (
	"context"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

// sender implements eventbus.Sender over the shared channel.
//
// It holds the shutdown signal alongside the channel rather than relying on the
// channel being closed. Close used to close(ch) directly, which made a Send racing
// Close a "send on closed channel" panic — on a goroutine this package does not
// own, during a PLANNED shutdown. A publisher blocked on a full buffer is exactly
// the case that hits it, and a caller had no way to establish that every Send had
// returned. Signaling on a separate channel keeps the send side panic-free and
// lets Send report ErrClosed.
type sender struct {
	ch   chan<- message
	done <-chan struct{}
}

func (s sender) Send(ctx context.Context, meta *event.Metadata, data []byte) error {
	if ctx == nil {
		return ErrNilContext
	} else if meta == nil {
		return ErrNilMetadata
	}

	// Checked BEFORE the blocking select, for the reason the receiver documents:
	// Go picks a ready case pseudo-randomly, so a select listing both done and a
	// buffer with space would accept messages after Close roughly half the time.
	select {
	case <-s.done:
		return ErrClosed
	default:
	}

	m := message{
		meta: meta,
		data: data,
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrClosed
	case s.ch <- m:
		return nil
	}
}
