package event

import (
	"context"
	"net/url"
	"time"
)

type Metadata struct {
	SpecVersion     string
	Type            string
	Source          string
	Subject         string
	ID              string
	Time            time.Time
	Extensions      map[string]interface{}
	DataSchema      *url.URL
	DataContentType string
}

func NewMetadata(t string) *Metadata {
	return &Metadata{
		SpecVersion: "1.0",
		Type:        t,
	}
}

type mdIncomingKey struct{}

func NewIncomingContext(ctx context.Context, md *Metadata) context.Context {
	return context.WithValue(ctx, mdIncomingKey{}, md)
}

func MetadataFromIncomingContext(ctx context.Context) (*Metadata, bool) {
	md, ok := ctx.Value(mdIncomingKey{}).(*Metadata)
	if !ok {
		return nil, false
	}

	return md, true
}
