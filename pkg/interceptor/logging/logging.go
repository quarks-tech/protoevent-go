package logging

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
)

func PublisherInterceptor(logger *logrus.Logger) eventbus.PublisherInterceptor {
	return func(ctx context.Context, name string, e interface{}, p *eventbus.PublisherImpl, pf eventbus.PublishFn, opts ...eventbus.PublishOption) error {
		err := pf(ctx, name, e, p, opts...)
		if err == nil {
			return nil
		}

		logger.
			WithField("eventName", name).
			WithField("body", fmt.Sprintf("%+v", e)).
			Errorf("handling event (%s): %s", name, err)

		return err
	}
}

func SubscriberInterceptor(logger *logrus.Logger) eventbus.SubscriberInterceptor {
	return func(ctx context.Context, md *event.Metadata, e interface{}, handler eventbus.Handler) error {
		iCtx := event.NewIncomingContext(ctx, md)
		hErr := handler(iCtx, e)
		if hErr != nil {
			logger.
				WithField("eventName", md.Type).
				WithField("body", fmt.Sprintf("%+v", e)).
				Errorf("error while handling event %s: %s", md.Type, hErr)
		}

		return hErr
	}
}
