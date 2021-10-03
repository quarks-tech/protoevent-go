package eventbus

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/quarks-tech/protoevent-go/pkg/encoding"
	"github.com/quarks-tech/protoevent-go/pkg/event"
)

type Sender interface {
	Send(ctx context.Context, metadata *event.Metadata, data []byte) error
}

type Publisher interface {
	Publish(ctx context.Context, name string, event interface{}, opts ...PublishOption) error
}

type PublishOption func(m *event.Metadata)

type publisherOptions struct {
	publishOptions    []PublishOption
	chainInterceptors []PublisherInterceptor
	interceptor       PublisherInterceptor
}

func defaultPublisherOptions() publisherOptions {
	return publisherOptions{}
}

type PublisherOption func(opts *publisherOptions)

type PublisherImpl struct {
	sender  Sender
	options publisherOptions
}

func NewPublisher(sender Sender, opts ...PublisherOption) *PublisherImpl {
	options := defaultPublisherOptions()

	for _, opt := range opts {
		opt(&options)
	}

	p := &PublisherImpl{
		sender:  sender,
		options: options,
	}

	chainPublisherInterceptors(p)

	return p
}

func (p *PublisherImpl) Publish(ctx context.Context, name string, event interface{}, opts ...PublishOption) error {
	opts = combine(p.options.publishOptions, opts)

	if p.options.interceptor != nil {
		return p.options.interceptor(ctx, name, event, p, publish, opts...)
	}

	return publish(ctx, name, event, p, opts...)
}

func publish(ctx context.Context, name string, e interface{}, p *PublisherImpl, opts ...PublishOption) error {
	md := newMetadata(name)

	for _, opt := range opts {
		opt(md)
	}

	contentSubtype, ok := event.ContentSubtype(md.DataContentType)
	if !ok {
		// @todo add error
	}

	codec, err := encoding.GetCodec(contentSubtype)
	if err != nil {
		return err
	}

	data, err := codec.Marshal(e)
	if err != nil {
		return fmt.Errorf(": %w", err)
	}

	if err = p.sender.Send(ctx, md, data); err != nil {
		return fmt.Errorf(": %w", err)
	}

	return nil
}

func combine(o1 []PublishOption, o2 []PublishOption) []PublishOption {
	// we don't use append because o1 could have extra capacity whose
	// elements would be overwritten, which could cause inadvertent
	// sharing (and race conditions) between concurrent calls
	if len(o1) == 0 {
		return o2
	} else if len(o2) == 0 {
		return o1
	}
	ret := make([]PublishOption, len(o1)+len(o2))
	copy(ret, o1)
	copy(ret[len(o1):], o2)
	return ret
}

func newMetadata(t string) *event.Metadata {
	return &event.Metadata{
		SpecVersion:     "1.0",
		Type:            t,
		ID:              uuid.New().String(),
		Time:            time.Now(),
		DataContentType: "application/cloudevents+proto",
	}
}
