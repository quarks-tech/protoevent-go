package cloudevent

import (
	"fmt"
	"time"

	"github.com/streadway/amqp"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
)

type Parser struct{}

func (p Parser) Parse(d *amqp.Delivery) (*event.Metadata, []byte, error) {
	md, err := p.parseMetadata(d)
	if err != nil {
		return nil, nil, eventbus.NewUnprocessableEventError(fmt.Errorf("parse amqp delivery: %w", err))
	}

	return md, d.Body, nil
}

func (Parser) parseMetadata(d *amqp.Delivery) (*event.Metadata, error) {
	if d.Type == "" {
		return nil, fmt.Errorf("required attribute 'type' is missing")
	}

	md := &event.Metadata{
		DataContentType: d.ContentType,
		Type:            d.Type,
	}

	if v, ok := d.Headers["cloudEvents:specversion"].(string); ok {
		md.SpecVersion = v
	} else {
		return nil, fmt.Errorf("required attribute 'specversion' is missing")
	}

	if v, ok := d.Headers["cloudEvents:id"].(string); ok {
		md.ID = v
	} else {
		return nil, fmt.Errorf("required attribute 'id' is missing")
	}

	if v, ok := d.Headers["cloudEvents:source"].(string); ok {
		md.Source = v
	} else {
		return nil, fmt.Errorf("required attribute 'source' is missing")
	}

	if v, ok := d.Headers["cloudEvents:subject"].(string); ok {
		md.Subject = v
	}

	if v, ok := d.Headers["cloudEvents:time"].(string); ok {
		var err error

		md.Time, err = time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, fmt.Errorf("parse time: %w", err)
		}
	}

	return md, nil
}
