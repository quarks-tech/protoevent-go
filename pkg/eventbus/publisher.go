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

// Sender is the transport seam on the publish side: one encoded event out.
// Implementations include the RabbitMQ sender, the in-memory gochan
// transport, and the transactional outbox Sender.
type Sender interface {
	Send(ctx context.Context, metadata *event.Metadata, data []byte) error
}

type Publisher interface {
	Publish(ctx context.Context, name string, e any, opts ...PublishOption) error
}

type PublishOption func(m *event.Metadata)

// IDGenerator produces a fresh event ID. The publisher has no pre-existing ID,
// so the generator takes no input. It is invoked only when the caller did not
// supply an ID via WithEventID.
type IDGenerator func() (string, error)

// generateUUIDv4 is the default IDGenerator. UUID v4 is random and unique, a
// sensible default for an event identifier. Producers that want a time-ordered
// or otherwise deterministic ID can supply one via WithEventID or WithIDGenerator.
func generateUUIDv4() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("eventbus: generate uuid v4 event id: %w", err)
	}

	return id.String(), nil
}

type publisherOptions struct {
	publishOptions    []PublishOption
	chainInterceptors []PublisherInterceptor
	interceptor       PublisherInterceptor
	idGenerator       IDGenerator
}

func defaultPublisherOptions() publisherOptions {
	return publisherOptions{
		idGenerator: generateUUIDv4,
	}
}

type PublisherOption func(opts *publisherOptions)

func WithEventID(id string) PublishOption {
	return func(m *event.Metadata) {
		m.ID = id
	}
}

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

func WithEventExtension(name string, value any) PublishOption {
	return func(m *event.Metadata) {
		if m.Extensions == nil {
			m.Extensions = make(map[string]any)
		}

		m.Extensions[name] = value
	}
}

func WithEventTime(t time.Time) PublishOption {
	return func(m *event.Metadata) {
		m.Time = t
	}
}

func WithPublisherContentType(t string) PublisherOption {
	return WithDefaultPublishOptions(WithEventContentType(t))
}

// WithIDGenerator sets the generator used to produce event IDs when the caller
// does not supply one via WithEventID. Defaults to UUID v4. A nil gen is
// ignored, keeping the default (installing nil would only mint a panic at the
// first Publish).
func WithIDGenerator(gen IDGenerator) PublisherOption {
	return func(opts *publisherOptions) {
		if gen != nil {
			opts.idGenerator = gen
		}
	}
}

func WithDefaultPublishOptions(pos ...PublishOption) PublisherOption {
	return func(opts *publisherOptions) {
		opts.publishOptions = append(opts.publishOptions, pos...)
	}
}

type PublisherImpl struct {
	sender  Sender
	options publisherOptions

	// defaultContentType and defaultCodec memoize the codec this publisher's own
	// configuration resolves to. Every publish otherwise re-parses the same content
	// type — a lower-casing scan, the prefix checks, the subtype token loop — and
	// re-hashes the result into the codec registry, for a value that has not changed
	// since NewPublisher. A per-event WithEventContentType override still takes the
	// full path. Both fields are written once here and only read afterwards, so
	// concurrent Publish calls need no synchronization; RegisterCodec is documented
	// as init-time only, so the memoized codec cannot go stale under us.
	defaultContentType string
	defaultCodec       encoding.Codec
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

	p.defaultContentType, p.defaultCodec = resolveDefaultCodec(options.publishOptions)

	chainPublisherInterceptors(p)

	return p
}

// resolveDefaultCodec replays the publisher's default publish options onto a
// scratch envelope to find the content type its events will carry, and resolves the
// codec for it. An unresolvable one is not an error here — NewPublisher has no way
// to report it — so it leaves the memo empty and publish reports it per event,
// exactly as it did before.
func resolveDefaultCodec(opts []PublishOption) (string, encoding.Codec) {
	var md event.Metadata
	for _, opt := range opts {
		opt(&md)
	}
	completeMetadata(&md)

	subtype, ok := event.ContentSubtype(md.DataContentType)
	if !ok {
		return "", nil
	}
	codec, err := encoding.GetCodec(subtype)
	if err != nil {
		return "", nil
	}

	return md.DataContentType, codec
}

// codecFor resolves the codec for one event's content type, taking the memoized
// answer when the event carries the publisher's configured content type.
func (p *PublisherImpl) codecFor(contentType string) (encoding.Codec, error) {
	if p.defaultCodec != nil && contentType == p.defaultContentType {
		return p.defaultCodec, nil
	}

	contentSubtype, ok := event.ContentSubtype(contentType)
	if !ok {
		return nil, fmt.Errorf("unsupported content type: %s", contentType)
	}

	codec, err := encoding.GetCodec(contentSubtype)
	if err != nil {
		return nil, fmt.Errorf("eventbus: publish: %w", err)
	}

	return codec, nil
}

func (p *PublisherImpl) Publish(ctx context.Context, name string, e any, opts ...PublishOption) error {
	opts = combine(p.options.publishOptions, opts)

	if p.options.interceptor != nil {
		return p.options.interceptor(ctx, name, e, p, publish, opts...)
	}

	return publish(ctx, name, e, p, opts...)
}

func publish(ctx context.Context, name string, e any, p *PublisherImpl, opts ...PublishOption) error {
	// Reject a malformed event type HERE, at the earliest point, because the
	// alternative is not a failed publish but a wedged consumer. Generated code
	// always emits "<service>.<Event>", but Publish is exported and callable with
	// anything; a dot-less type sails through the codec and the transport, and the
	// subscriber then rejects every such delivery as unprocessable. Worse over an
	// outbox: the row commits with the caller's business transaction, and the relay
	// can neither send it (the RabbitMQ sender needs the dot to split exchange from
	// routing key) nor classify it as poison — so the lane stops on that row every
	// tick forever and nothing behind it is delivered, recoverable only by editing
	// offsets in a live database. Failing the publish is the cheap end of that.
	if _, _, err := event.SplitType(name); err != nil {
		return fmt.Errorf("eventbus: publish: %w", err)
	}

	md := event.NewMetadata(name)

	for _, opt := range opts {
		opt(md)
	}

	if md.ID == "" {
		id, err := p.options.idGenerator()
		if err != nil {
			return fmt.Errorf("generate event id: %w", err)
		}

		md.ID = id
	}

	completeMetadata(md)

	// A schema whose URI is empty is no schema. `schema, _ := url.Parse(cfg.URL)`
	// on an empty config value yields a NON-nil &url.URL{}, and carrying that
	// forward makes the event's dataschema differ from the one every reader
	// reconstructs (the decoders map an empty URI back to nil). Normalize once,
	// here, so the in-process and post-store values agree.
	if md.DataSchema != nil && md.DataSchema.String() == "" {
		md.DataSchema = nil
	}

	// Reject an unusable extension here for the same reason as the type check
	// above: the alternative is not a failed publish but a wedged relay. The content
	// marshalers refuse such an extension too, but they run at SEND time — over an
	// outbox that is after the row has committed with the caller's business
	// transaction, and a marshal failure is not a DecodeError, so the lane stops on
	// that row every tick forever. Catching it at publish keeps the unsendable row
	// from being written at all.
	//
	// Both halves matter. A reserved NAME overwrites a core attribute in whichever
	// mode shares its namespace; a nested VALUE (map/struct/slice) is rejected by
	// the AMQP field encoder outright, which is the same wedge arriving through the
	// other half of the pair.
	for k, v := range md.Extensions {
		if event.ReservedExtensionName(k) {
			return fmt.Errorf("eventbus: publish: extension %q collides with a core CloudEvents attribute; rename it", k)
		}
		if err := event.ValidExtensionValue(v); err != nil {
			return fmt.Errorf("eventbus: publish: extension %q: %w", k, err)
		}
	}

	codec, err := p.codecFor(md.DataContentType)
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

	if md.Time.IsZero() {
		md.Time = time.Now()
	}
}
