package pubsub

import (
	"context"
	"reflect"

	"google.golang.org/grpc/grpclog"
)

var logger = grpclog.Component("pubsub")

type eventHandler func(cons interface{}, ctx context.Context, dec func(interface{}) error, interceptor SubscriberInterceptor) error

type SubscriberDesc struct {
	EventName string
	// The pointer to the subscriber interface. Used to check whether the user
	// provided implementation satisfies the interface requirements.
	HandlerType interface{}
	Handler     eventHandler
	Metadata    interface{}
}

type SubscriberRegistrar interface {
	RegisterSubscriber(desc *SubscriberDesc, impl interface{})
}

type Consumer struct {
}

func (c *Consumer) RegisterSubscriber(sd *SubscriberDesc, ss interface{}) {
	if ss != nil {
		ht := reflect.TypeOf(sd.HandlerType).Elem()
		st := reflect.TypeOf(ss)
		if !st.Implements(ht) {
			logger.Fatalf("pubsub: Consumer.RegisterSubscriber found the handler of type %v that does not satisfy %v", st, ht)
		}
	}
	c.register(sd, ss)
}

func (c *Consumer) register(sd *SubscriberDesc, ss interface{}) {

}
