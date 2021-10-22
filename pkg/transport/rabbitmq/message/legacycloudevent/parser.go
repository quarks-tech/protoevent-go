package legacycloudevent

import (
	"time"

	"github.com/streadway/amqp"

	"github.com/quarks-tech/protoevent-go/pkg/encoding"
	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
)

type Parser struct{}

func (Parser) Parse(d *amqp.Delivery) (*event.Metadata, []byte, error) {
	meta := &event.Metadata{
		SpecVersion:     d.Headers["cloudEvents:specversion"].(string),
		ID:              d.Headers["cloudEvents:id"].(string),
		DataContentType: d.ContentType,
		Type:            d.Type,
	}

	var err error

	meta.Time, err = time.Parse(time.RFC3339, d.Headers["cloudEvents:time"].(string))
	if err != nil {
		return nil, nil, eventbus.NewUnprocessableEventError(err)
	}

	codec, _ := encoding.GetCodec(meta.DataContentType)

	eventData := &legacyEventData{}
	err = codec.Unmarshal(d.Body, eventData)
	if err != nil {
		return nil, nil, eventbus.NewUnprocessableEventError(err)
	}

	return meta, []byte(eventData.Data), nil
}
