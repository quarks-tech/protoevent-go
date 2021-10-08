package main

import (
	"context"
	"fmt"
	"log"

	"github.com/quarks-tech/protoevent-go/example/gen/example/books/v1"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/gochan"
)

type Handler struct {
}

func (h Handler) HandleBookCreatedEvent(_ context.Context, e *books.BookCreatedEvent) error {
	fmt.Printf("Creating book with ID %d\n", e.Id)

	return nil
}

func main() {
	ctx := context.Background()

	sendReceiver := gochan.New()

	publisher := eventbus.NewPublisher(sendReceiver)
	booksPublisher := books.NewEventPublisher(publisher)

	subscriber := eventbus.NewSubscriber("example.consumers.v1")
	books.RegisterBookCreatedEventHandler(subscriber, Handler{})

	for c := int32(1); c <= 10; c++ {
		err := booksPublisher.PublishBookCreatedEvent(ctx, &books.BookCreatedEvent{
			Id: c,
		})
		if err != nil {
			log.Fatal(err)
		}
	}

	sendReceiver.Close(ctx)

	if err := subscriber.Subscribe(ctx, sendReceiver); err != nil {
		log.Fatal(err)
	}
}
