package validator

import (
	"context"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
)

type validatable interface {
	ValidateAll() error
}

func validate(event interface{}) error {
	if v, ok := event.(validatable); ok {
		return v.ValidateAll()
	}

	return nil
}

func SubscriberInterceptor() eventbus.SubscriberInterceptor {
	return func(ctx context.Context, md *event.Metadata, event interface{}, handler eventbus.Handler) error {
		if err := validate(event); err != nil {
			return eventbus.NewUnprocessableEventError(err)
		}

		return handler(ctx, event)
	}
}
