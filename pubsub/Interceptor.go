package pubsub

import "context"

type SubscriberInfo struct {
	Subscriber interface{}
	EventName  string
}

type Handler func(ctx context.Context, req interface{}) error

type SubscriberInterceptor func(ctx context.Context, data interface{}, info *SubscriberInfo, handler Handler) error
