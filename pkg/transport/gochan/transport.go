// Package gochan is the in-memory transport: sender and receiver share one
// buffered Go channel. It exists for tests and single-process wiring — no
// durability, no redelivery, no cross-process reach.
package gochan

import (
	"context"
	"errors"
	"sync"

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

	// ErrClosed is returned by Send after Close. It replaces the "send on closed
	// channel" panic a Send racing Close used to raise.
	ErrClosed = errors.New("gochan: transport closed")
)

// SendReceiver is both ends of the in-memory transport over one shared
// channel: it satisfies eventbus.Sender, eventbus.Receiver, and
// eventbus.Setuper.
type SendReceiver struct {
	sender   sender
	receiver receiver

	// done is closed by Close and is the shutdown signal for both ends. The shared
	// message channel is never closed, so no Send can panic on it.
	done      chan struct{}
	closeOnce sync.Once
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
	done := make(chan struct{})

	sr := &SendReceiver{
		sender:   sender{ch: ch, done: done},
		receiver: receiver{ch: ch, done: done},
		done:     done,
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

// Close signals shutdown, ending Receive once the buffer drains. It is safe to
// call concurrently with Send and safe to call more than once.
//
// Close used to close the shared message channel, which made a second Close — or a
// Send racing Close — a panic, waived as "acceptable for this in-memory test
// transport". It is not: gochan is the documented single-process wiring and the
// transport used in the end-to-end tests, a publisher blocked on a full buffer is
// exactly what a shutdown interrupts, and the API offered a caller no barrier with
// which to prove every Send had returned. A Send after Close now returns ErrClosed.
func (sr *SendReceiver) Close(_ context.Context) {
	sr.closeOnce.Do(func() { close(sr.done) })
}
