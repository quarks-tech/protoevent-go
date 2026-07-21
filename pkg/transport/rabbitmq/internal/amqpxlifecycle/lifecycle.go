// Package amqpxlifecycle adapts long-lived RabbitMQ consumers to the bounded
// cancellation semantics of amqpx commands.
package amqpxlifecycle

import (
	"context"
	"io"
	"sync"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/amqpx"
	"github.com/quarks-tech/amqpx/connpool"
)

// ProcessWithDrain lets ctx cancel connection acquisition, but once a command
// attempt starts it keeps the borrowed connection alive until that attempt
// returns. The command must observe the caller's shutdown context separately,
// stop accepting deliveries, and drain its in-flight work before returning.
func ProcessWithDrain(
	ctx context.Context,
	process func(context.Context, amqpx.Command) error,
	command amqpx.Command,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	processCtx, cancelProcess := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelProcess()

	var (
		stateMu         sync.Mutex
		commandRunning  bool
		shutdownDrained bool
	)

	stopCancel := context.AfterFunc(ctx, func() {
		stateMu.Lock()
		shouldCancel := !commandRunning && !shutdownDrained
		stateMu.Unlock()

		if shouldCancel {
			cancelProcess()
		}
	})
	defer stopCancel()

	err := process(processCtx, func(commandCtx context.Context, conn *connpool.Conn) (err error) {
		stateMu.Lock()
		if ctxErr := ctx.Err(); ctxErr != nil {
			stateMu.Unlock()

			return ctxErr
		}
		commandRunning = true
		stateMu.Unlock()

		defer func() {
			stateMu.Lock()
			commandRunning = false
			ctxErr := ctx.Err()
			shutdownDrained = ctxErr != nil
			stateMu.Unlock()

			if ctxErr != nil {
				err = ctxErr
			}
		}()

		return command(commandCtx, conn)
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	return err
}

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
		return connErr
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
