package message

import (
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/message/contentmode/binary"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/message/contentmode/structured"
)

const structuredContentType = "application/cloudevents+json"

type Marshaler struct {
	structured structured.Marshaler
	binary     binary.Marshaler
}

func (m Marshaler) Marshal(md *event.Metadata, data []byte) (amqp.Publishing, error) {
	if md.DataContentType == structuredContentType {
		return m.structured.Marshal(md, data)
	}

	return m.binary.Marshal(md, data)
}

func (m Marshaler) Unmarshal(d *amqp.Delivery) (*event.Metadata, []byte, error) {
	if d.ContentType == structuredContentType {
		return m.structured.Unmarshal(d)
	}

	return m.binary.Unmarshal(d)
}
