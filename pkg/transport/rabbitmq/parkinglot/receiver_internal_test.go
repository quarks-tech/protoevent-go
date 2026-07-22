package parkinglot

import (
	"context"
	"errors"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/amqpx/connpool"
)

// TestPutIntoParkingLotDetachesCanceledContext pins the drain-mode contract:
// under amqpx ProcessWithDrain the command ctx IS the canceled shutdown ctx,
// and amqp091's PublishWithContext fast-path rejects a canceled ctx before
// touching the channel. putIntoParkingLot must detach (context.WithoutCancel)
// so the publish is actually attempted during drain. With a zero-value
// channel, an ATTEMPTED publish panics — which is exactly the evidence the
// fast-path was bypassed; a context.Canceled error return means the detach
// regressed.
func TestPutIntoParkingLotDetachesCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	receiver := &Receiver{
		options: receiverOptions{dlxExchange: "events.dlx"},
	}
	conn := connpool.NewConn(nil, &amqp.Channel{})

	var err error
	panicked := func() (panicked bool) {
		defer func() { panicked = recover() != nil }()
		err = receiver.putIntoParkingLot(ctx, conn, &amqp.Delivery{})
		return false
	}()

	if errors.Is(err, context.Canceled) {
		t.Fatalf("putIntoParkingLot() error = %v: canceled ctx reached the publish — WithoutCancel detach regressed", err)
	}
	if !panicked {
		t.Fatalf("expected the zero-value channel publish to be attempted (panic); got err = %v", err)
	}
}
