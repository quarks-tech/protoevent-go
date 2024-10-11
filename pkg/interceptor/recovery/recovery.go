package recovery

import (
	"context"
	"fmt"
	"runtime"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
)

func PublisherInterceptor(h RecoveryHandlerFuncContext) eventbus.PublisherInterceptor {
	return func(ctx context.Context, name string, e interface{}, p *eventbus.PublisherImpl, pf eventbus.PublishFn, opts ...eventbus.PublishOption) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = recoverFrom(ctx, r, h)
			}
		}()

		return pf(ctx, name, e, p, opts...)
	}
}

func SubscriberInterceptor(h RecoveryHandlerFuncContext) eventbus.SubscriberInterceptor {
	return func(ctx context.Context, md *event.Metadata, e interface{}, handler eventbus.Handler) (err error) {
		iCtx := event.NewIncomingContext(ctx, md)
		defer func() {
			if r := recover(); r != nil {
				err = recoverFrom(ctx, r, h)
			}
		}()

		return handler(iCtx, e)
	}
}

type RecoveryHandlerFuncContext func(ctx context.Context, p any) (err error)

func recoverFrom(ctx context.Context, p any, r RecoveryHandlerFuncContext) error {
	if r != nil {
		return r(ctx, p)
	}
	stack := make([]byte, 64<<10)
	stack = stack[:runtime.Stack(stack, false)]
	return &PanicError{Panic: p, Stack: stack}
}

type PanicError struct {
	Panic any
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("panic caught: %v\n\n%s", e.Panic, e.Stack)
}
