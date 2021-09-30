package eventbus

import "context"

type PublishFn func(ctx context.Context, name string, e interface{}, p *PublisherImpl, opts ...PublishOption) error

type PublisherInterceptor func(ctx context.Context, name string, e interface{}, p *PublisherImpl, pf PublishFn, opts ...PublishOption) error

func WithPublisherInterceptor(f PublisherInterceptor) PublisherOption {
	return func(o *publisherOptions) {
		o.interceptor = f
	}
}

func WithChainPublisherInterceptor(interceptors ...PublisherInterceptor) PublisherOption {
	return func(o *publisherOptions) {
		o.chainInterceptors = append(o.chainInterceptors, interceptors...)
	}
}

func chainPublisherInterceptors(cc *PublisherImpl) {
	interceptors := cc.options.chainInterceptors
	// Prepend opts.interceptor to the chaining interceptors if it exists, since interceptor will
	// be executed before any other chained interceptors.
	if cc.options.interceptor != nil {
		interceptors = append([]PublisherInterceptor{cc.options.interceptor}, interceptors...)
	}
	var chainedInt PublisherInterceptor
	if len(interceptors) == 0 {
		chainedInt = nil
	} else if len(interceptors) == 1 {
		chainedInt = interceptors[0]
	} else {
		chainedInt = func(ctx context.Context, name string, event interface{}, cc *PublisherImpl, invoker PublishFn, opts ...PublishOption) error {
			return interceptors[0](ctx, name, event, cc, getChainUnaryInvoker(interceptors, 0, invoker), opts...)
		}
	}
	cc.options.interceptor = chainedInt
}

func getChainUnaryInvoker(interceptors []PublisherInterceptor, curr int, finalInvoker PublishFn) PublishFn {
	if curr == len(interceptors)-1 {
		return finalInvoker
	}
	return func(ctx context.Context, name string, event interface{}, cc *PublisherImpl, opts ...PublishOption) error {
		return interceptors[curr+1](ctx, name, event, cc, getChainUnaryInvoker(interceptors, curr+1, finalInvoker), opts...)
	}
}
