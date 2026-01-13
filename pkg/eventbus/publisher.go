package eventbus

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/encoding"
	"github.com/quarks-tech/protoevent-go/pkg/encoding/proto"
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

func WithEventContentType(contentType string) PublishOption {
	return func(m *event.Metadata) {
		m.DataContentType = strings.ToLower(contentType)
	}
}

func WithEventSource(source string) PublishOption {
	return func(m *event.Metadata) {
		m.Source = source
	}
}

func WithEventSubject(subject string) PublishOption {
	return func(m *event.Metadata) {
		m.Subject = subject
	}
}

func WithEventDataSchema(schema *url.URL) PublishOption {
	return func(m *event.Metadata) {
		m.DataSchema = schema
	}
}

func WithEventExtension(name string, value interface{}) PublishOption {
	return func(m *event.Metadata) {
		if m.Extensions == nil {
			m.Extensions = make(map[string]interface{})
		}

		m.Extensions[name] = value
	}
}

func WithPublisherContentType(t string) PublisherOption {
	return WithDefaultPublishOptions(WithEventContentType(t))
}

func WithDefaultPublishOptions(pos ...PublishOption) PublisherOption {
	return func(opts *publisherOptions) {
		opts.publishOptions = append(opts.publishOptions, pos...)
	}
}

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
	md := event.NewMetadata(name)

	for _, opt := range opts {
		opt(md)
	}

	completeMetadata(md)

	contentSubtype, ok := event.ContentSubtype(md.DataContentType)
	if !ok {
		return fmt.Errorf("unsupported content type: %s", md.DataContentType)
	}

	codec, err := encoding.GetCodec(contentSubtype)
	if err != nil {
		return err
	}

	data, err := codec.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal : %w", err)
	}

	if err = p.sender.Send(ctx, md, data); err != nil {
		return fmt.Errorf("send : %w", err)
	}

	return nil
}

func combine(o1, o2 []PublishOption) []PublishOption {
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

func completeMetadata(md *event.Metadata) {
	if md.DataContentType == "" {
		md.DataContentType = event.ContentType(proto.Name)
	}

	if md.Source == "" {
		md.Source = "protoevent-go"
	}

	if md.ID == "" {
		md.ID = uuid.New().String()
	}

	if md.Time.IsZero() {
		md.Time = time.Now()
	}
}
