package binary

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

type Marshaler struct{}

func (m Marshaler) Marshal(md *event.Metadata, data []byte) (amqp.Publishing, error) {
	return amqp.Publishing{
		Type:        md.Type,
		ContentType: md.DataContentType,
		Headers:     marshalMetadata(md),
		Body:        data,
	}, nil
}

func (m Marshaler) Unmarshal(d *amqp.Delivery) (*event.Metadata, []byte, error) {
	md, err := unmarshalMetadata(d)
	if err != nil {
		return nil, nil, fmt.Errorf("parse amqp delivery: %w", err)
	}

	return md, d.Body, nil
}

func marshalMetadata(meta *event.Metadata) amqp.Table {
	headers := amqp.Table{
		"cloudEvents:specversion": meta.SpecVersion,
		"cloudEvents:id":          meta.ID,
		"cloudEvents:source":      meta.Source,
	}

	if meta.Subject != "" {
		headers["cloudEvents:subject"] = meta.Subject
	}

	if meta.DataSchema != nil {
		headers["cloudEvents:dataschema"] = meta.DataSchema.String()
	}

	if !meta.Time.IsZero() {
		headers["cloudEvents:time"] = meta.Time.Format(time.RFC3339)
	}

	if meta.Extensions != nil {
		for k, v := range meta.Extensions {
			headers[k] = v
		}
	}

	return headers
}

func unmarshalMetadata(d *amqp.Delivery) (*event.Metadata, error) {
	if d.Type == "" {
		return nil, errors.New("required attribute 'type' is missing")
	}

	md := &event.Metadata{
		DataContentType: d.ContentType,
		Type:            d.Type,
	}

	v, ok := d.Headers["cloudEvents:specversion"].(string)
	if !ok {
		return nil, errors.New("required attribute 'specversion' is missing")
	}
	md.SpecVersion = v

	v, ok = d.Headers["cloudEvents:id"].(string)
	if !ok {
		return nil, errors.New("required attribute 'id' is missing")
	}
	md.ID = v

	v, ok = d.Headers["cloudEvents:source"].(string)
	if !ok {
		return nil, errors.New("required attribute 'source' is missing")
	}
	md.Source = v

	if v, ok := d.Headers["cloudEvents:subject"].(string); ok {
		md.Subject = v
	}

	if v, ok := d.Headers["cloudEvents:time"].(string); ok {
		var err error

		md.Time, err = time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, fmt.Errorf("parse attribute 'time': %w", err)
		}
	}

	if v, ok := d.Headers["cloudEvents:dataschema"].(string); ok {
		var err error

		md.DataSchema, err = url.Parse(v)
		if err != nil {
			return nil, fmt.Errorf("parse attribute 'dataschema': %w", err)
		}
	}

	for k, v := range d.Headers {
		if !strings.HasPrefix(k, "cloudEvents") {
			if md.Extensions == nil {
				md.Extensions = make(map[string]interface{})
			}

			md.Extensions[k] = v
		}
	}

	return md, nil
}
