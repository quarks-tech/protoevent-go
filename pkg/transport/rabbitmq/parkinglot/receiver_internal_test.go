package parkinglot

import (
	"context"
	"errors"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/amqpx/connpool"
)

func TestPutIntoParkingLotPassesCommandContextToPublish(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	receiver := &Receiver{
		options: receiverOptions{dlxExchange: "events.dlx"},
	}
	conn := connpool.NewConn(nil, &amqp.Channel{})

	err := receiver.putIntoParkingLot(ctx, conn, &amqp.Delivery{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("putIntoParkingLot() error = %v, want context.Canceled", err)
	}
}
