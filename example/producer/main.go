package main

import (
	"context"
	"fmt"
	"log"

	"github.com/quarks-tech/protoevent-go/example/gen/example/books/v1"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq"
	"github.com/streadway/amqp"
	"golang.org/x/sync/errgroup"
)

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
		MinIdleConns: 4,
	})

	defer client.Close()

	sender := rabbitmq.NewSender(client)
	publisher := eventbus.NewPublisher(sender)
	booksPublisher := books.NewEventPublisher(publisher)

	var eg errgroup.Group

	for i := int32(1); i <= 4; i++ {
		i := i
		eg.Go(func() error {
			fmt.Println("start publisher: ", i)

			for c := int32(1); c <= 1000; c++ {
				err := booksPublisher.PublishBookCreatedEvent(context.Background(), &books.BookCreatedEvent{
					Id: c * i,
				})
				if err != nil {
					fmt.Println(err)
				}
			}

			fmt.Println("stop publisher: ", i)

			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		log.Fatal(err)
	}
}
