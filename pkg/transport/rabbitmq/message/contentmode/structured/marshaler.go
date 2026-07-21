package structured

import (
	"errors"
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
		return amqp.Publishing{}, fmt.Errorf("marshal cloudevents envelope: %w", err)
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
		return nil, nil, fmt.Errorf("unmarshal cloudevents envelope: %w", err)
	}

	md := new(event.Metadata)

	// require extracts (and consumes) a mandatory envelope attribute; the
	// leftover dto entries become extensions below.
	require := func(key string) (string, error) {
		raw, ok := dto[key]
		if !ok {
			return "", fmt.Errorf("required attribute '%s' is missing", key)
		}
		delete(dto, key)
		return strings.Trim(string(raw), "\""), nil
	}

	var err error
	if md.SpecVersion, err = require("specversion"); err != nil {
		return nil, nil, err
	}
	if md.Type, err = require("type"); err != nil {
		return nil, nil, err
	}
	if md.ID, err = require("id"); err != nil {
		return nil, nil, err
	}
	if md.Source, err = require("source"); err != nil {
		return nil, nil, err
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

	// data is required too, but stays raw bytes: no quote-trimming.
	rawData, ok := dto["data"]
	if !ok {
		return nil, nil, errors.New("required attribute 'data' is missing")
	}
	data := []byte(rawData)
	delete(dto, "data")

	for k, raw := range dto {
		if md.Extensions == nil {
			md.Extensions = make(map[string]interface{})
		}

		var v interface{}

		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, nil, fmt.Errorf("unmarshal extension attribute %q: %w", k, err)
		}

		md.Extensions[k] = v
	}

	return md, data, nil
}
