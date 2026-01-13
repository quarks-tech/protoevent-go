package rabbitmq

import (
	stdamqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

type Marshaler interface {
	Unmarshal(d *stdamqp.Delivery) (*event.Metadata, []byte, error)
	Marshal(md *event.Metadata, data []byte) (stdamqp.Publishing, error)
}
