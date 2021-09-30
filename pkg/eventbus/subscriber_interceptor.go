package eventbus

import (
	"context"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

type Handler func(ctx context.Context, event interface{}) error

type SubscriberInterceptor func(ctx context.Context, md *event.Metadata, event interface{}, handler Handler) error

func WithSubscriberInterceptor(f SubscriberInterceptor) SubscriberOption {
	return func(o *subscriberOptions) {
		o.interceptor = f
	}
}

func WithChainSubscriberInterceptor(interceptors ...SubscriberInterceptor) SubscriberOption {
	return func(o *subscriberOptions) {
		o.chainInterceptors = append(o.chainInterceptors, interceptors...)
	}
}

func chainSubscriberInterceptors(s *Subscriber) {
	// Prepend opts.interceptor to the chaining interceptors if it exists, since interceptor will
	// be executed before any other chained interceptors.
	interceptors := s.opts.chainInterceptors
	if s.opts.interceptor != nil {
		interceptors = append([]SubscriberInterceptor{s.opts.interceptor}, s.opts.chainInterceptors...)
	}

	var chainedInt SubscriberInterceptor
	if len(interceptors) == 0 {
		chainedInt = nil
	} else if len(interceptors) == 1 {
		chainedInt = interceptors[0]
	} else {
		chainedInt = chainInterceptors(interceptors)
	}

	s.opts.interceptor = chainedInt
}

func chainInterceptors(interceptors []SubscriberInterceptor) SubscriberInterceptor {
	return func(ctx context.Context, md *event.Metadata, event interface{}, handler Handler) error {
		var i int
		var next Handler
		next = func(ctx context.Context, event interface{}) error {
			if i == len(interceptors)-1 {
				return interceptors[i](ctx, md, event, handler)
			}
			i++
			return interceptors[i-1](ctx, md, event, next)
		}
		return next(ctx, event)
	}
}
