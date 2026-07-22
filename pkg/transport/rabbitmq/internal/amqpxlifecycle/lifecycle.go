// Package amqpxlifecycle provides the graceful-shutdown choreography for
// long-lived RabbitMQ consumers: multiplexing the stop reasons (application
// shutdown, worker failure, broker-initiated close) and draining prefetched
// deliveries so Channel.Cancel can complete. Connection lifetime across
// cancellation is handled by amqpx itself (Client.ProcessWithDrain, since
// amqpx v0.3.2) — this package only owns what happens on the channel.
package amqpxlifecycle

import (
	"context"
	"io"

	amqp "github.com/rabbitmq/amqp091-go"
)

// WaitForConsumerStop cancels the AMQP consumer when either application
// shutdown or the delivery worker fails. A completed worker needs no further
// cancellation. The borrowed connection remains owned by amqpx and is released
// after the command returns.
func WaitForConsumerStop(
	shutdownCtx context.Context,
	workerFailed <-chan struct{},
	workerDone <-chan struct{},
	cancelConsumer func() error,
	notifyClose <-chan *amqp.Error,
) error {
	select {
	case <-shutdownCtx.Done():
		return cancelConsumer()
	case <-workerFailed:
		return cancelConsumer()
	case <-workerDone:
		return nil
	case connErr := <-notifyClose:
		// A clean AMQP shutdown CLOSES the notify channel without sending, so
		// the receive yields a nil *amqp.Error — returning that directly would
		// wrap a nil pointer in a non-nil error interface and make orderly
		// closure look like a failure to the errgroup.
		if connErr != nil {
			return connErr
		}

		return nil
	}
}

// DrainDeliveries processes deliveries until the stream closes. After the
// first processing failure it signals the control goroutine and keeps draining
// without processing so Channel.Cancel can complete without blocking.
func DrainDeliveries(
	groupCtx context.Context,
	shutdownCtx context.Context,
	deliveries <-chan amqp.Delivery,
	workerFailed chan<- struct{},
	process func(context.Context, *amqp.Delivery) error,
) error {
	var processErr error

	for {
		select {
		case <-groupCtx.Done():
			return processErr
		case delivery, ok := <-deliveries:
			if !ok {
				switch {
				case processErr != nil:
					return processErr
				case shutdownCtx.Err() != nil:
					return nil
				default:
					return io.ErrUnexpectedEOF
				}
			}
			if groupCtx.Err() != nil {
				return processErr
			}

			if processErr != nil {
				continue
			}

			if err := process(groupCtx, &delivery); err != nil {
				processErr = err
				close(workerFailed)
			}
		}
	}
}
