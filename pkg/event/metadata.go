package event

import (
	"context"
	"time"
)

type Metadata struct {
	SpecVersion     string
	Type            string
	Source          string
	ID              string
	Time            time.Time
	Extensions      map[string]string
	DataContentType string
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
