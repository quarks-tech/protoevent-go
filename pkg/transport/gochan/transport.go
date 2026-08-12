// Package gochan is the in-memory transport: sender and receiver share one
// buffered Go channel. It exists for tests and single-process wiring — no
// durability, no redelivery, no cross-process reach.
package gochan

import (
	"context"
	"errors"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
)

const (
	defaultChanDepth = 20
)

// Sentinel errors returned by Send on invalid input.
var (
	ErrNilContext  = errors.New("nil Context")
	ErrNilMetadata = errors.New("nil Metadata")
)

// SendReceiver is both ends of the in-memory transport over one shared
// channel: it satisfies eventbus.Sender, eventbus.Receiver, and
// eventbus.Setuper.
type SendReceiver struct {
	sender   sender
	receiver receiver
}

type message struct {
	meta *event.Metadata
	data []byte
}

// Option configures a SendReceiver.
type Option func(*SendReceiver)

// WithErrorHandler installs a sink for handler failures.
//
// This transport has no redelivery and no dead-letter queue, so an event whose
// handler returns an error is dropped and delivery continues (ending the
// subscription instead would silently stop every event behind it). Without a
// handler that drop is invisible, which is why installing one is recommended for
// anything beyond a test: h is called on the receive goroutine, so it must not
// block.
func WithErrorHandler(h func(md *event.Metadata, err error)) Option {
	return func(sr *SendReceiver) { sr.receiver.onErr = h }
}

// New builds a SendReceiver over a fresh channel (buffer depth 20).
func New(opts ...Option) *SendReceiver {
	ch := make(chan message, defaultChanDepth)

	sr := &SendReceiver{
		sender:   ch,
		receiver: receiver{ch: ch},
	}

	for _, opt := range opts {
		opt(sr)
	}

	return sr
}

// Setup implements eventbus.Setuper. It is a no-op: an in-memory channel has
// no topology to declare.
func (sr *SendReceiver) Setup(ctx context.Context, serviceName string, infos ...eventbus.ServiceInfo) error {
	return sr.receiver.Setup(ctx, serviceName, infos...)
}

// Send implements eventbus.Sender. It blocks while the channel buffer is full.
func (sr *SendReceiver) Send(ctx context.Context, meta *event.Metadata, data []byte) error {
	return sr.sender.Send(ctx, meta, data)
}

// Receive implements eventbus.Receiver: it delivers buffered events to
// processor until the channel is closed (Close) or ctx is canceled.
func (sr *SendReceiver) Receive(ctx context.Context, processor eventbus.Processor) error {
	return sr.receiver.Receive(ctx, processor)
}

// Close closes the underlying channel, ending Receive after the buffer
// drains. Close exactly once, and only after every Send has returned: a
// second Close — or a Send racing Close — panics (bare channel semantics,
// acceptable for this in-memory test transport).
func (sr *SendReceiver) Close(_ context.Context) {
	close(sr.sender)
}
