package eventbus

import (
	"context"
	"fmt"
	"reflect"
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

type EventDesc struct {
	Name        string
	HandlerType interface{}
	Handler     eventHandler
}

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
	handler     eventHandler
	handlerImpl interface{}
}

type serviceInfo struct {
	events map[string]*eventInfo
	mdata  string
}

type eventHandler func(h interface{}, md *event.Metadata, ctx context.Context, dec func(interface{}) error, inter SubscriberInterceptor) error

type EventHandlerRegistrar interface {
	RegisterEventHandler(desc *ServiceDesc, event string, impl interface{})
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

func (s *Subscriber) RegisterEventHandler(sd *ServiceDesc, eventName string, h interface{}) {
	ed, ok := sd.getEventDesc(eventName)
	if !ok {
		panicf("event not found: %s", eventName)
	}

	if h != nil {
		sht := reflect.TypeOf(ed.HandlerType).Elem()
		ht := reflect.TypeOf(h)
		if !ht.Implements(sht) {
			panicf("Subscriber.RegisterSubscription found the handlerImpl of type %v that does not satisfy %v", ht, sht)
		}
	}

	s.register(sd, ed, h)
}

func (s *Subscriber) register(sd *ServiceDesc, ed EventDesc, h interface{}) {
	s.mux.Lock()
	defer s.mux.Unlock()

	if s.serve {
		panicf("Subscriber.RegisterEventHandler after Subscriber.Subscribe for %q", ed.Name)
	}

	if _, ok := s.services[sd.ServiceName]; !ok {
		s.services[sd.ServiceName] = &serviceInfo{
			events: make(map[string]*eventInfo),
			mdata:  sd.Metadata,
		}
	}

	if _, ok := s.services[sd.ServiceName].events[ed.Name]; ok {
		panicf("Subscriber.RegisterEventHandler found duplicate service registration for %q", ed.Name)
	}

	s.services[sd.ServiceName].events[ed.Name] = &eventInfo{
		handler:     ed.Handler,
		handlerImpl: h,
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

	df := func(v interface{}) error {
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

	return ei.handler(ei.handlerImpl, md, ctx, df, s.opts.interceptor)
}

func panicf(format string, a ...interface{}) {
	panic(fmt.Sprintf("eventbus: "+format, a...))
}
