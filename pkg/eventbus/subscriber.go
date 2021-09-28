package eventbus

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/quarks-tech/protoevent-go/pkg/encoding"
	"github.com/quarks-tech/protoevent-go/pkg/event"
	"google.golang.org/grpc/grpclog"
)

var logger = grpclog.Component("eventbus")

type Receiver interface {
	Setup(ctx context.Context, infos ...ServiceInfo) error
	Receive(ctx context.Context, fn func(md *event.Metadata, data []byte) error) error
}

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

func (sd ServiceDesc) findEvent(name string) (EventDesc, bool) {
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

type SubscriberInfo struct {
	Subscriber interface{}
	EventName  string
}

type Handler func(ctx context.Context, attrs interface{}) error

type SubscriberInterceptor func(ctx context.Context, data interface{}, info *SubscriberInfo, handler Handler) error

type eventHandler func(cons interface{}, ctx context.Context, dec func(interface{}) error, interceptor SubscriberInterceptor) error

type SubscriptionDesc struct {
	EventName string
	// The pointer to the subscriber interface. Used to check whether the user
	// provided implementation satisfies the interface requirements.
	HandlerType interface{}
	Handler     eventHandler
	Metadata    interface{}
}

type SubscriptionRegistrar interface {
	RegisterSubscription(desc *ServiceDesc, event string, impl interface{})
}

type subscriberOptions struct {
	interceptor       SubscriberInterceptor
	chainInterceptors []SubscriberInterceptor
}

func defaultSubscriberOptions() *subscriberOptions {
	return &subscriberOptions{}
}

type SubscriberOption func(opts *subscriberOptions)

type Subscriber struct {
	mux      sync.Mutex
	options  *subscriberOptions
	services map[string]*serviceInfo
	serve    bool
	workers  int
}

func NewSubscriber(opts ...SubscriberOption) *Subscriber {
	defOpts := defaultSubscriberOptions()

	for _, opt := range opts {
		opt(defOpts)
	}

	return &Subscriber{
		options:  defOpts,
		services: make(map[string]*serviceInfo),
	}
}

func (s *Subscriber) RegisterSubscription(sd *ServiceDesc, eventName string, h interface{}) {
	ed, ok := sd.findEvent(eventName)
	if !ok {
		logger.Fatalf("find event")
	}

	if h != nil {
		sht := reflect.TypeOf(ed.HandlerType).Elem()
		ht := reflect.TypeOf(h)
		if !ht.Implements(sht) {
			logger.Fatalf("eventbus: Subscriber.RegisterSubscription found the handlerImpl of type %v that does not satisfy %v", ht, sht)
		}
	}
	s.register(sd, ed, h)
}

func (s *Subscriber) register(sd *ServiceDesc, ed EventDesc, h interface{}) {
	s.mux.Lock()
	defer s.mux.Unlock()

	logger.Infof("eventbus: RegisterSubscription(%q)", "")

	if s.serve {
		logger.Fatalf("eventbus: Subscriber.RegisterSubscription after Subscriber.SubscribeAndConsume for %q", "")
	}

	if _, ok := s.services[sd.ServiceName]; !ok {
		s.services[sd.ServiceName] = &serviceInfo{
			events: make(map[string]*eventInfo),
			mdata:  sd.Metadata,
		}
	}

	if _, ok := s.services[sd.ServiceName].events[ed.Name]; ok {
		logger.Fatalf("eventbus: Subscriber.RegisterSubscription found duplicate service registration for %q", "")
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

	for name, service := range s.services {
		si := ServiceInfo{
			ServiceName: name,
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

	if err := r.Setup(ctx, s.GetServiceInfo()...); err != nil {
		return err
	}

	return r.Receive(ctx, s.process)
}

func (s *Subscriber) process(md *event.Metadata, data []byte) error {
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

	ctx := event.NewIncomingContext(context.Background(), md)

	return ei.handler(ei.handlerImpl, ctx, df, s.options.interceptor)
}
