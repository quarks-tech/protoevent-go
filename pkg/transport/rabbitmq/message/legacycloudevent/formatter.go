package legacycloudevent

import (
	"time"

	"github.com/streadway/amqp"

	"github.com/quarks-tech/protoevent-go/pkg/encoding"
	"github.com/quarks-tech/protoevent-go/pkg/event"
)

type Formatter struct{}

type legacyEventData struct {
	Data            string `json:"data"`
	DataContentType string `json:"datacontenttype"`
	ID              string `json:"id"`
	Source          string `json:"source"`
	SpecVersion     string `json:"specversion"`
	Time            string `json:"time"`
	TraceParent     string `json:"traceparent"`
	Type            string `json:"type"`
}

func createEventData(meta *event.Metadata, data []byte) *legacyEventData {
	return &legacyEventData{
		Data:            string(data),
		DataContentType: meta.DataContentType,
		ID:              meta.ID,
		Source:          meta.Source,
		SpecVersion:     meta.SpecVersion,
		Time:            meta.Time.Format(time.RFC3339),
		TraceParent:     "",
		Type:            meta.Type,
	}
}

func (Formatter) Format(meta *event.Metadata, data []byte) amqp.Publishing {
	contentSubtype, _ := event.ContentSubtype(meta.DataContentType)
	codec, _ := encoding.GetCodec(contentSubtype)

	eventData := createEventData(meta, data)
	body, _ := codec.Marshal(eventData)

	return amqp.Publishing{
		Type:        meta.Type,
		ContentType: meta.DataContentType,
		Headers:     buildPublishingHeaders(meta),
		Body:        body,
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
