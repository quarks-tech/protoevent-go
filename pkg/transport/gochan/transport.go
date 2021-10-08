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

var (
	ErrNilContext  = errors.New("nil Context")
	ErrNilMetadata = errors.New("nil Metadata")
)

type SendReceiver struct {
	sender   sender
	receiver receiver
}

type message struct {
	meta *event.Metadata
	data []byte
}

func New() *SendReceiver {
	ch := make(chan message, defaultChanDepth)

	return &SendReceiver{
		sender:   ch,
		receiver: ch,
	}
}

func (sr *SendReceiver) Setup(ctx context.Context, serviceName string, infos ...eventbus.ServiceInfo) error {
	return sr.receiver.Setup(ctx, serviceName, infos...)
}

func (sr *SendReceiver) Send(ctx context.Context, meta *event.Metadata, data []byte) error {
	return sr.sender.Send(ctx, meta, data)
}

func (sr *SendReceiver) Receive(ctx context.Context, processor eventbus.Processor) error {
	return sr.receiver.Receive(ctx, processor)
}

func (sr *SendReceiver) Close(_ context.Context) {
	close(sr.sender)
}
