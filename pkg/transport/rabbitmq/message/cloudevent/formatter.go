package cloudevent

import (
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/streadway/amqp"
)

type Formatter struct{}

func (Formatter) Format(meta *event.Metadata, data []byte) amqp.Publishing {
	return amqp.Publishing{
		Type:        meta.Type,
		ContentType: meta.DataContentType,
		Headers:     buildPublishingHeaders(meta),
		Body:        data,
	}
}

func buildPublishingHeaders(meta *event.Metadata) amqp.Table {
	return amqp.Table{
		"cloudEvents:time":        meta.Time.Format(time.RFC3339),
		"cloudEvents:id":          meta.ID,
		"cloudEvents:specversion": meta.SpecVersion,
		"cloudEvents:source":      meta.Source,
	}
}
