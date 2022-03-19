package recovery

import "context"

var defaultOptions = &options{
	handlerFunc: nil,
}

type options struct {
	handlerFunc HandlerFuncContext
}

func evaluateOptions(opts []Option) *options {
	optCopy := &options{}
	*optCopy = *defaultOptions
	for _, o := range opts {
		o(optCopy)
	}
	return optCopy
}

type Option func(*options)

func WithHandler(f HandlerFunc) Option {
	return func(o *options) {
		o.handlerFunc = HandlerFuncContext(func(ctx context.Context, p interface{}) error {
			return f(p)
		})
	}
}

func WithHandlerContext(f HandlerFuncContext) Option {
	return func(o *options) {
		o.handlerFunc = f
	}
}
