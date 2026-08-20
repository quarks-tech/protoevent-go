package eventbus

import "context"

type PublishFn func(ctx context.Context, name string, e any, p *PublisherImpl, opts ...PublishOption) error

type PublisherInterceptor func(ctx context.Context, name string, e any, p *PublisherImpl, pf PublishFn, opts ...PublishOption) error

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

func chainPublisherInterceptors(p *PublisherImpl) {
	interceptors := p.options.chainInterceptors
	// Prepend opts.interceptor to the chaining interceptors if it exists, since interceptor will
	// be executed before any other chained interceptors.
	if p.options.interceptor != nil {
		interceptors = append([]PublisherInterceptor{p.options.interceptor}, interceptors...)
	}
	var chainedInt PublisherInterceptor
	switch len(interceptors) {
	case 0:
		chainedInt = nil
	case 1:
		chainedInt = interceptors[0]
	default:
		chainedInt = func(ctx context.Context, name string, e any, p *PublisherImpl, invoker PublishFn, opts ...PublishOption) error {
			return interceptors[0](ctx, name, e, p, chainInvoker(interceptors, 0, invoker), opts...)
		}
	}
	p.options.interceptor = chainedInt
}

func chainInvoker(interceptors []PublisherInterceptor, curr int, finalInvoker PublishFn) PublishFn {
	if curr == len(interceptors)-1 {
		return finalInvoker
	}
	return func(ctx context.Context, name string, e any, p *PublisherImpl, opts ...PublishOption) error {
		return interceptors[curr+1](ctx, name, e, p, chainInvoker(interceptors, curr+1, finalInvoker), opts...)
	}
}
