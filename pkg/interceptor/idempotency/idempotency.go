package idempotency

import (
	"context"
	"fmt"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
)

func SubscriberInterceptor(checker UniqueChecker) eventbus.SubscriberInterceptor {
	return func(ctx context.Context, md *event.Metadata, e interface{}, handler eventbus.Handler) error {
		uniq, err := checker(ctx, md.ID)
		if err != nil {
			return fmt.Errorf("checking event [%s] id [%s] idempotency: %w", md.Type, md.ID, err)
		}

		if !uniq {
			return eventbus.NewUnprocessableEventError(fmt.Errorf("event [%s] with id[%s] is not unique", md.Type, md.ID))
		}

		iCtx := event.NewIncomingContext(ctx, md)

		return handler(iCtx, e)
	}
}

type UniqueChecker func(ctx context.Context, id string) (bool, error)
