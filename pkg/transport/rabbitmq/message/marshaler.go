package message

import (
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/message/contentmode/binary"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/message/contentmode/structured"
)

type Marshaler struct {
	structured structured.Marshaler
	binary     binary.Marshaler
}

// isStructured reports whether a content type selects the CloudEvents structured
// envelope this package can write and read.
//
// Dispatch goes through event.ParseContentType — the ONE definition of what a
// content type means here — rather than a local comparison. A local one matched
// "application/cloudevents+json" and its parameters, but not
// "application/cloudevents;json", which the shared parser accepts and which the
// publisher's own codec lookup resolves to JSON; such a delivery went to the BINARY
// unmarshaler, found no cloudEvents:* headers, and was failed as unprocessable —
// parking every event from that publisher in the DLQ.
//
// The subtype guard stays: the structured marshaler here implements only the JSON
// envelope, so "application/cloudevents+proto" must keep falling through to binary
// rather than be handed to a marshaler that would emit JSON under a proto content
// type.
func isStructured(contentType string) bool {
	mode, subtype, ok := event.ParseContentType(contentType)

	return ok && mode == event.StructuredMode && subtype == "json"
}

func (m Marshaler) Marshal(md *event.Metadata, data []byte) (amqp.Publishing, error) {
	if isStructured(md.DataContentType) {
		return m.structured.Marshal(md, data)
	}

	return m.binary.Marshal(md, data)
}

func (m Marshaler) Unmarshal(d *amqp.Delivery) (*event.Metadata, []byte, error) {
	if isStructured(d.ContentType) {
		return m.structured.Unmarshal(d)
	}

	return m.binary.Unmarshal(d)
}
