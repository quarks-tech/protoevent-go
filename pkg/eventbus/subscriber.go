package eventbus

import (
	"context"
	"fmt"
	"sync"

	"github.com/quarks-tech/protoevent-go/pkg/encoding"
	"github.com/quarks-tech/protoevent-go/pkg/event"
)

// Receiver is the transport seam on the subscribe side: it blocks delivering
// incoming events to p until ctx is canceled, the transport fails, or the
// transport shuts down cleanly. A clean shutdown returns nil — a
// drain-capable transport (the rabbitmq receiver) finishes in-flight
// deliveries after ctx cancellation and reports the planned stop as success,
// not as context.Canceled.
type Receiver interface {
	Receive(ctx context.Context, p Processor) error
}

// Setuper is the optional transport capability for declaring topology
// (exchanges, queues, bindings) before receiving; transports without setup
// needs simply don't implement it.
type Setuper interface {
	Setup(ctx context.Context, serviceName string, info ...ServiceInfo) error
}

// Processor consumes one raw incoming event (CloudEvents metadata + encoded
// payload). A non-nil error tells the transport the event was not handled,
// and transport-specific redelivery/parking semantics apply.
//
// ctx is the per-delivery context and becomes the handler's own: the transport
// supplies the narrowest context that still covers this delivery. It is NOT
// canceled at the first shutdown signal, so an in-flight handler still gets its
// drain window.
//
// Do NOT treat ctx as a shutdown deadline. For the rabbitmq receivers it is
// amqpx's consumer-group context, which amqpx derives from context.Background()
// and cancels only when the group's stop watcher itself fails — a CLEAN shutdown
// and an expired DrainTimeout both leave it live (amqpx v0.3.5 consume.go:224,
// client.go:204). A handler that blocks on ctx.Done() to abandon work therefore
// never wakes on those paths; it may finish against a connection that is already
// gone, and its Ack is lost, so the delivery is redelivered after restart. Bound
// long handler work with a timeout of your own, and make the work idempotent —
// which at-least-once delivery requires regardless.
type Processor func(ctx context.Context, md *event.Metadata, data []byte) error

// EventHandler handles a single event. The handler implementation is captured
// in the closure, so no any-typed value is passed at runtime.
//
// Parameters:
//   - ctx: request context
//   - md: event metadata (CloudEvents)
//   - dec: decoder function to unmarshal event data
//   - inter: optional interceptor chain (may be nil)
type EventHandler func(ctx context.Context, md *event.Metadata, dec func(any) error, inter SubscriberInterceptor) error

// EventDesc describes a single event type.
type EventDesc struct {
	Name string
}

// ServiceDesc describes a service and its events.
// Used for topology setup (e.g., RabbitMQ exchanges/queues).
type ServiceDesc struct {
	ServiceName string
	Events      []EventDesc
	Metadata    string
}

func (sd ServiceDesc) eventDesc(name string) (EventDesc, bool) {
	for _, ed := range sd.Events {
		if ed.Name == name {
			return ed, true
		}
	}

	return EventDesc{}, false
}

type eventInfo struct {
	handler EventHandler
}

type serviceInfo struct {
	events map[string]*eventInfo
	mdata  string
}

type subscriberOptions struct {
	interceptor       SubscriberInterceptor
	chainInterceptors []SubscriberInterceptor
}

func defaultSubscriberOptions() subscriberOptions {
	return subscriberOptions{}
}

type SubscriberOption func(opts *subscriberOptions)

type Subscriber struct {
	mu       sync.Mutex
	name     string
	opts     subscriberOptions
	services map[string]*serviceInfo
	serve    bool
}

func NewSubscriber(name string, opts ...SubscriberOption) *Subscriber {
	options := defaultSubscriberOptions()

	for _, opt := range opts {
		opt(&options)
	}

	s := &Subscriber{
		opts:     options,
		name:     name,
		services: make(map[string]*serviceInfo),
	}

	chainSubscriberInterceptors(s)

	return s
}

// RegisterHandler registers an event handler for the given service and event.
// Type safety is ensured at compile time by the generated registration functions.
//
// This method is typically called by generated code like:
//
//	bookspb.RegisterBookCreatedEventHandler(subscriber, &MyHandler{})
//
// The generated function creates a closure that captures the typed handler,
// eliminating the need for runtime type checking.
func (s *Subscriber) RegisterHandler(sd *ServiceDesc, eventName string, h EventHandler) {
	ed, ok := sd.eventDesc(eventName)
	if !ok {
		panicf("event not found: %s", eventName)
	}

	s.register(sd, ed, h)
}

func (s *Subscriber) register(sd *ServiceDesc, ed EventDesc, h EventHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.serve {
		panicf("Subscriber.RegisterHandler after Subscriber.Subscribe for %q", ed.Name)
	}

	if _, ok := s.services[sd.ServiceName]; !ok {
		s.services[sd.ServiceName] = &serviceInfo{
			events: make(map[string]*eventInfo),
			mdata:  sd.Metadata,
		}
	}

	if _, ok := s.services[sd.ServiceName].events[ed.Name]; ok {
		panicf("Subscriber.RegisterHandler found duplicate service registration for %q", ed.Name)
	}

	s.services[sd.ServiceName].events[ed.Name] = &eventInfo{
		handler: h,
	}
}

type ServiceInfo struct {
	ServiceName string
	Events      []string
}

func (s *Subscriber) GetServiceInfo() []ServiceInfo {
	sInfos := make([]ServiceInfo, 0, len(s.services))

	for sName, service := range s.services {
		si := ServiceInfo{
			ServiceName: sName,
			Events:      make([]string, 0, len(service.events)),
		}

		for eName := range service.events {
			si.Events = append(si.Events, eName)
		}

		sInfos = append(sInfos, si)
	}

	return sInfos
}

func (s *Subscriber) Subscribe(ctx context.Context, r Receiver) error {
	s.mu.Lock()
	s.serve = true
	s.mu.Unlock()

	if setuper, ok := r.(Setuper); ok {
		if err := setuper.Setup(ctx, s.name, s.GetServiceInfo()...); err != nil {
			return fmt.Errorf("eventbus: subscribe: setup topology: %w", err)
		}
	}

	if err := r.Receive(ctx, s.process); err != nil {
		return fmt.Errorf("eventbus: subscribe: %w", err)
	}

	return nil
}

// process is the Processor the transport drives. ctx comes FROM the transport
// (see Processor): deriving it from context.Background() instead — which this
// used to do — left handlers with no way to observe shutdown at all, so under a
// drain-capable receiver a slow handler ran past the drain budget, the
// connection was force-closed under it, and the unacked delivery was redelivered
// after restart — running any non-idempotent side effect twice.
func (s *Subscriber) process(ctx context.Context, md *event.Metadata, data []byte) error {
	// md.Type arrives from the incoming message, so it is untrusted: a dot-less
	// type must be rejected as unprocessable, never sliced (that panics).
	service, eventName, err := event.SplitType(md.Type)
	if err != nil {
		return NewUnprocessableEventError(err)
	}

	srv, knownService := s.services[service]
	if !knownService {
		return NewUnprocessableEventError(fmt.Errorf("subscription not found: %s", md.Type))
	}

	ei, ok := srv.events[eventName]
	if !ok {
		return NewUnprocessableEventError(fmt.Errorf("subscription not found: %s", md.Type))
	}

	df := func(v any) error {
		contentSubtype, valid := event.ContentSubtype(md.DataContentType)
		if !valid {
			return NewUnprocessableEventError(fmt.Errorf("invalid content type: %s", md.DataContentType))
		}

		codec, err := encoding.GetCodec(contentSubtype)
		if err != nil {
			return NewUnprocessableEventError(fmt.Errorf("get codec %s: %w", contentSubtype, err))
		}

		if err = codec.Unmarshal(data, v); err != nil {
			return NewUnprocessableEventError(fmt.Errorf("unmarshalling event data: %w", err))
		}

		return nil
	}

	ctx = event.NewIncomingContext(ctx, md)

	return ei.handler(ctx, md, df, s.opts.interceptor)
}

func panicf(format string, a ...any) {
	panic(fmt.Sprintf("eventbus: "+format, a...))
}
