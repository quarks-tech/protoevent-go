package eventbus

import (
	"context"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

type Handler func(ctx context.Context, e any) error

type SubscriberInterceptor func(ctx context.Context, md *event.Metadata, e any, handler Handler) error

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
	switch len(interceptors) {
	case 0:
		chainedInt = nil
	case 1:
		chainedInt = interceptors[0]
	default:
		chainedInt = chainInterceptors(interceptors)
	}

	s.opts.interceptor = chainedInt
}

func chainInterceptors(interceptors []SubscriberInterceptor) SubscriberInterceptor {
	return func(ctx context.Context, md *event.Metadata, e any, handler Handler) error {
		return interceptors[0](ctx, md, e, chainHandler(interceptors, 0, md, handler))
	}
}

// chainHandler builds interceptor curr's next-Handler with an IMMUTABLE
// position — each nesting level owns its index (mirroring the publisher
// chain's chainInvoker), so an interceptor that calls next more than once (a
// retry interceptor) re-runs the SAME downstream chain. The previous
// closure-shared mutable index made the second pass skip every interceptor
// between the caller and the tail.
func chainHandler(interceptors []SubscriberInterceptor, curr int, md *event.Metadata, finalHandler Handler) Handler {
	if curr == len(interceptors)-1 {
		return finalHandler
	}
	return func(ctx context.Context, e any) error {
		return interceptors[curr+1](ctx, md, e, chainHandler(interceptors, curr+1, md, finalHandler))
	}
}
