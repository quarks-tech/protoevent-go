package amqpxlifecycle

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/amqpx"
	"github.com/quarks-tech/amqpx/connpool"
)

func TestProcessWithDrainCancelsBeforeCommandStarts(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	acquiring := make(chan struct{})
	result := make(chan error, 1)
	var commandCalled atomic.Bool

	go func() {
		result <- ProcessWithDrain(
			ctx,
			func(processCtx context.Context, _ amqpx.Command) error {
				close(acquiring)
				<-processCtx.Done()

				return processCtx.Err()
			},
			func(context.Context, *connpool.Conn) error {
				commandCalled.Store(true)

				return nil
			},
		)
	}()

	<-acquiring
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ProcessWithDrain() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ProcessWithDrain did not cancel connection acquisition")
	}

	if commandCalled.Load() {
		t.Fatal("command ran after cancellation during connection acquisition")
	}
}

func TestProcessWithDrainStartsCanceledWhenCallerIsAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var processCalled atomic.Bool
	var commandCalled atomic.Bool
	err := ProcessWithDrain(
		ctx,
		func(processCtx context.Context, _ amqpx.Command) error {
			processCalled.Store(true)

			return processCtx.Err()
		},
		func(context.Context, *connpool.Conn) error {
			commandCalled.Store(true)

			return nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ProcessWithDrain() error = %v, want context.Canceled", err)
	}
	if processCalled.Load() {
		t.Fatal("process ran for an already-canceled caller")
	}
	if commandCalled.Load() {
		t.Fatal("command ran for an already-canceled caller")
	}
}

func TestProcessWithDrainKeepsLeaseUntilRunningCommandReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	commandStarted := make(chan struct{})
	shutdownObserved := make(chan struct{})
	releaseCommand := make(chan struct{})
	result := make(chan error, 1)
	var processContextCanceledEarly atomic.Bool

	go func() {
		result <- ProcessWithDrain(
			ctx,
			func(processCtx context.Context, command amqpx.Command) error {
				err := command(processCtx, nil)
				if processCtx.Err() != nil {
					processContextCanceledEarly.Store(true)
				}

				return err
			},
			func(processCtx context.Context, _ *connpool.Conn) error {
				close(commandStarted)

				select {
				case <-processCtx.Done():
					return errors.New("process context canceled before the running command drained")
				case <-ctx.Done():
				}

				if processCtx.Err() != nil {
					return errors.New("process context canceled while the running command was draining")
				}
				close(shutdownObserved)
				<-releaseCommand

				return nil
			},
		)
	}()

	<-commandStarted
	cancel()
	select {
	case <-shutdownObserved:
	case err := <-result:
		t.Fatalf("ProcessWithDrain returned before observing shutdown: %v", err)
	case <-time.After(time.Second):
		t.Fatal("running command did not observe caller cancellation")
	}

	select {
	case err := <-result:
		t.Fatalf("ProcessWithDrain returned before the running command drained: %v", err)
	default:
	}

	close(releaseCommand)

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ProcessWithDrain() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ProcessWithDrain did not return after the command drained")
	}
	if processContextCanceledEarly.Load() {
		t.Fatal("process context canceled before the command callback returned to Process")
	}
}

func TestWaitForConsumerStopCancelsConsumerWhenWorkerFails(t *testing.T) {
	workerFailed := make(chan struct{})
	close(workerFailed)

	var cancelCalled atomic.Bool
	err := WaitForConsumerStop(
		t.Context(),
		workerFailed,
		make(chan struct{}),
		func() error {
			cancelCalled.Store(true)

			return nil
		},
		make(chan *amqp.Error),
	)
	if err != nil {
		t.Fatalf("WaitForConsumerStop() error = %v, want nil", err)
	}
	if !cancelCalled.Load() {
		t.Fatal("WaitForConsumerStop did not cancel the consumer")
	}
}

func TestDrainDeliveriesSignalsFailureAndContinuesDraining(t *testing.T) {
	deliveries := make(chan amqp.Delivery, 2)
	deliveries <- amqp.Delivery{DeliveryTag: 1}
	deliveries <- amqp.Delivery{DeliveryTag: 2}
	close(deliveries)

	wantErr := errors.New("ack failed")
	workerFailed := make(chan struct{})
	processed := 0
	err := DrainDeliveries(
		t.Context(),
		t.Context(),
		deliveries,
		workerFailed,
		func(context.Context, *amqp.Delivery) error {
			processed++

			return wantErr
		},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("DrainDeliveries() error = %v, want %v", err, wantErr)
	}
	if processed != 1 {
		t.Fatalf("processed deliveries = %d, want 1", processed)
	}

	select {
	case <-workerFailed:
	default:
		t.Fatal("DrainDeliveries did not signal the processing failure")
	}
}

func TestDrainDeliveriesReturnsUnexpectedEOFWhenStreamCloses(t *testing.T) {
	deliveries := make(chan amqp.Delivery)
	close(deliveries)

	err := DrainDeliveries(
		t.Context(),
		t.Context(),
		deliveries,
		make(chan struct{}),
		func(context.Context, *amqp.Delivery) error {
			t.Fatal("processor called for a closed delivery stream")

			return nil
		},
	)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("DrainDeliveries() error = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestDrainDeliveriesDoesNotProcessAfterGroupCancellation(t *testing.T) {
	processed := 0

	for range 100 {
		groupCtx, cancelGroup := context.WithCancel(t.Context())
		cancelGroup()

		deliveries := make(chan amqp.Delivery, 1)
		deliveries <- amqp.Delivery{}

		_ = DrainDeliveries(
			groupCtx,
			t.Context(),
			deliveries,
			make(chan struct{}),
			func(context.Context, *amqp.Delivery) error {
				processed++

				return nil
			},
		)
	}

	if processed != 0 {
		t.Fatalf("processed deliveries after cancellation = %d, want 0", processed)
	}
}
