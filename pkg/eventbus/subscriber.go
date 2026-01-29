package eventbus

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/quarks-tech/protoevent-go/pkg/encoding"
	"github.com/quarks-tech/protoevent-go/pkg/event"
)

type Receiver interface {
	Receive(ctx context.Context, p Processor) error
}

type Setuper interface {
	Setup(ctx context.Context, serviceName string, info ...ServiceInfo) error
}

type Processor func(md *event.Metadata, data []byte) error

// EventHandler handles a single event. The handler implementation is captured
// in the closure, so no interface{} is passed at runtime.
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

func (sd ServiceDesc) getEventDesc(name string) (EventDesc, bool) {
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
	mux      sync.Mutex
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
	ed, ok := sd.getEventDesc(eventName)
	if !ok {
		panicf("event not found: %s", eventName)
	}

	s.register(sd, ed, h)
}

func (s *Subscriber) register(sd *ServiceDesc, ed EventDesc, h EventHandler) {
	s.mux.Lock()
	defer s.mux.Unlock()

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
	s.mux.Lock()
	s.serve = true
	s.mux.Unlock()

	if setuper, ok := r.(Setuper); ok {
		if err := setuper.Setup(ctx, s.name, s.GetServiceInfo()...); err != nil {
			return err
		}
	}

	return r.Receive(ctx, s.process)
}

func (s *Subscriber) process(md *event.Metadata, data []byte) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pos := strings.LastIndex(md.Type, ".")
	service := md.Type[:pos]
	eventName := md.Type[pos+1:]

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
