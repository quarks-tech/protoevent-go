package structured

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	json "github.com/json-iterator/go"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

type Marshaler struct{}

func (m Marshaler) Marshal(md *event.Metadata, data []byte) (amqp.Publishing, error) {
	dto := map[string]interface{}{
		"specversion": md.SpecVersion,
		"id":          md.ID,
		"type":        md.Type,
		"source":      md.Source,
		"data":        json.RawMessage(data),
	}

	if md.DataContentType != "" {
		dto["datacontenttype"] = md.DataContentType
	}

	if md.DataSchema != nil {
		dto["dataschema"] = md.DataSchema.String()
	}

	if md.Subject != "" {
		dto["subject"] = md.Subject
	}

	if !md.Time.IsZero() {
		dto["time"] = md.Time.Format(time.RFC3339)
	}

	if md.Extensions != nil {
		for k, v := range md.Extensions {
			dto[k] = v
		}
	}

	body, err := json.Marshal(&dto)
	if err != nil {
		return amqp.Publishing{}, err
	}

	return amqp.Publishing{
		Type:        md.Type,
		ContentType: md.DataContentType,
		Body:        body,
	}, nil
}

func (m Marshaler) Unmarshal(d *amqp.Delivery) (*event.Metadata, []byte, error) {
	dto := make(map[string]json.RawMessage)

	if err := json.Unmarshal(d.Body, &dto); err != nil {
		return nil, nil, err
	}

	md := new(event.Metadata)

	if raw, ok := dto["specversion"]; ok {
		md.SpecVersion = strings.Trim(string(raw), "\"")

		delete(dto, "specversion")
	} else {
		return nil, nil, fmt.Errorf("required attribute 'specversion' is missing")
	}

	if raw, ok := dto["type"]; ok {
		md.Type = strings.Trim(string(raw), "\"")

		delete(dto, "type")
	} else {
		return nil, nil, fmt.Errorf("required attribute 'type' is missing")
	}

	if raw, ok := dto["id"]; ok {
		md.ID = strings.Trim(string(raw), "\"")

		delete(dto, "id")
	} else {
		return nil, nil, fmt.Errorf("required attribute 'id' is missing")
	}

	if raw, ok := dto["source"]; ok {
		md.Source = strings.Trim(string(raw), "\"")

		delete(dto, "source")
	} else {
		return nil, nil, fmt.Errorf("required attribute 'source' is missing")
	}

	if raw, ok := dto["subject"]; ok {
		md.Subject = strings.Trim(string(raw), "\"")

		delete(dto, "subject")
	}

	if raw, ok := dto["time"]; ok {
		var err error

		md.Time, err = time.Parse(time.RFC3339, strings.Trim(string(raw), "\""))
		if err != nil {
			return nil, nil, fmt.Errorf("parse attribute 'time': %w", err)
		}

		delete(dto, "time")
	}

	if raw, ok := dto["dataschema"]; ok {
		var err error

		md.DataSchema, err = url.Parse(strings.Trim(string(raw), "\""))
		if err != nil {
			return nil, nil, fmt.Errorf("parse attribute 'dataschema': %w", err)
		}

		delete(dto, "dataschema")
	}

	if raw, ok := dto["datacontenttype"]; ok {
		md.DataContentType = strings.Trim(string(raw), "\"")

		delete(dto, "datacontenttype")
	}

	var data []byte

	if raw, ok := dto["data"]; ok {
		data = raw

		delete(dto, "data")
	} else {
		return nil, nil, fmt.Errorf("required attribute 'data' is missing")
	}

	for k, raw := range dto {
		if md.Extensions == nil {
			md.Extensions = make(map[string]interface{})
		}

		var v interface{}

		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, nil, err
		}

		md.Extensions[k] = v
	}

	return md, data, nil
}
