package message

import (
	"strings"

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

// isStructured reports whether a content type selects CloudEvents structured
// mode.
//
// The comparison ignores RFC 2045 parameters and case, because "media type" and
// "the exact bytes some publisher sent" are not the same thing:
// "application/cloudevents+json; charset=utf-8" is a legal spelling of structured
// mode, and matching it byte-for-byte routes it to the BINARY unmarshaler, which
// finds no cloudEvents:* headers and fails it as unprocessable — parking every
// event from that publisher in the DLQ. Media types are case-insensitive per RFC
// 2045 §5.1 for the same reason.
func isStructured(contentType string) bool {
	base, _, _ := strings.Cut(contentType, ";")

	return strings.EqualFold(strings.TrimSpace(base), structuredContentType)
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
