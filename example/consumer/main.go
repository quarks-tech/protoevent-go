package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/streadway/amqp"

	"github.com/quarks-tech/protoevent-go/example/gen/example/books/v1"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq"
)

type Handler struct{}

func (h Handler) HandleBookCreatedEvent(ctx context.Context, e *books.BookCreatedEvent) error {
	fmt.Printf("%d\n", e.Id)

	return nil
}

func main() {
	client := rabbitmq.NewClient(&rabbitmq.Config{
		Address: "localhost:5672",
		AMQP: amqp.Config{
			Vhost: "/",
			SASL: []amqp.Authentication{
				&amqp.PlainAuth{
					Username: "guest",
					Password: "guest",
				},
			},
		},
		MinIdleConns: 3,
	})

	defer client.Close()

	receiver := rabbitmq.NewReceiver(client,
		rabbitmq.WithTopologySetup(),
		rabbitmq.WithDLX(),
	)

	subscriber := eventbus.NewSubscriber("example.consumers.v1")
	books.RegisterBookCreatedEventHandler(subscriber, Handler{})

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		<-sigs
		cancel()
	}()

	if err := subscriber.Subscribe(ctx, receiver); err != nil {
		log.Fatal(err)
	}
}
