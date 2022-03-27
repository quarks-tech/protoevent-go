package timeout

import (
	"context"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
)

func SubscriberInterceptor(timeout time.Duration) eventbus.SubscriberInterceptor {
	return func(ctx context.Context, md *event.Metadata, event interface{}, handler eventbus.Handler) error {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		return handler(ctx, event)
	}
}
