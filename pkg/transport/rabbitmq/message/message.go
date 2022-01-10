package message

import (
	"github.com/streadway/amqp"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

type Formatter interface {
	Format(md *event.Metadata, data []byte) amqp.Publishing
}

type Parser interface {
	Parse(d *amqp.Delivery) (*event.Metadata, []byte, error)
}
