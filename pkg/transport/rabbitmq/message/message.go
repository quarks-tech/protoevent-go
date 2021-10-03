package message

import (
	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/streadway/amqp"
)

type Formatter interface {
	Format(md *event.Metadata, data []byte) amqp.Publishing
}

type Parser interface {
	Parse(d *amqp.Delivery) (*event.Metadata, []byte, error)
}
