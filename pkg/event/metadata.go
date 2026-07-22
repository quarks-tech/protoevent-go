// Package event holds the CloudEvents 1.0 metadata model shared by every
// transport: the Metadata envelope, its context plumbing for subscriber
// handlers, and content-type parsing for codec selection.
package event

import (
	"context"
	"net/url"
	"time"
)

// Metadata is the CloudEvents 1.0 event envelope
// (https://github.com/cloudevents/spec). Publishers fill Type/Source/etc. via
// eventbus publish options; the bus completes ID and Time when the caller
// leaves them zero. Extensions values round-trip through JSON in some stores,
// so numeric extension values come back as float64.
type Metadata struct {
	SpecVersion     string
	Type            string
	Source          string
	Subject         string
	ID              string
	Time            time.Time
	Extensions      map[string]any
	DataSchema      *url.URL
	DataContentType string
}

// NewMetadata returns a Metadata for event type t with SpecVersion pinned to
// CloudEvents 1.0; every other field is left for the caller (or the bus's
// metadata completion) to fill.
func NewMetadata(t string) *Metadata {
	return &Metadata{
		SpecVersion: "1.0",
		Type:        t,
	}
}

type mdIncomingKey struct{}

// NewIncomingContext returns a ctx carrying md. Transports call it before
// invoking subscriber handlers, so handler code can recover the envelope via
// MetadataFromIncomingContext.
func NewIncomingContext(ctx context.Context, md *Metadata) context.Context {
	return context.WithValue(ctx, mdIncomingKey{}, md)
}

// MetadataFromIncomingContext returns the incoming event's Metadata stored in
// ctx by NewIncomingContext, and whether one was present.
func MetadataFromIncomingContext(ctx context.Context) (*Metadata, bool) {
	md, ok := ctx.Value(mdIncomingKey{}).(*Metadata)
	if !ok {
		return nil, false
	}

	return md, true
}
