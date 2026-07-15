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

// New builds a SendReceiver over a fresh channel (buffer depth 20).
func New() *SendReceiver {
	ch := make(chan message, defaultChanDepth)

	return &SendReceiver{
		sender:   ch,
		receiver: ch,
	}
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
