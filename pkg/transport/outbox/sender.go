package outbox

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

// IDGenerator produces the outbox row ID for a message. It receives the event
// metadata so a generator may derive the ID from it — in particular,
// ReuseMetadataID returns the event's own ID.
type IDGenerator func(md *event.Metadata) (string, error)

// GenerateV4 is the default IDGenerator. It ignores the metadata and mints a
// fresh random UUID v4 as the outbox row's unique primary key, independent of
// the caller-controlled Metadata.ID.
//
// v4 (random) is deliberate: the relay orders by create_time, not by this ID,
// so a time-ordered ID gives no ordering benefit — and on TiDB a monotonic
// primary key concentrates inserts on one Region (a write hotspot). A random
// key scatters inserts across Regions. See storage ADR 012.
func GenerateV4(_ *event.Metadata) (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}

	return id.String(), nil
}

// ReuseMetadataID uses the event's Metadata.ID as the outbox row ID. This gives
// the row and the event a single identity end to end.
//
// The relay orders pending rows by create_time, not by ID, so reusing the event
// ID does not affect delivery ordering regardless of its format. The only
// requirement is that Metadata.ID be unique (it is the row's primary key).
func ReuseMetadataID(md *event.Metadata) (string, error) {
	return md.ID, nil
}

type senderOptions struct {
	idGenerator IDGenerator
}

func defaultSenderOptions() senderOptions {
	return senderOptions{
		idGenerator: ReuseMetadataID,
	}
}

// SenderOption configures an outbox Sender.
type SenderOption func(opts *senderOptions)

// WithIDGenerator sets the generator used to produce the outbox row ID.
// Defaults to GenerateV4. Use ReuseMetadataID to key rows on the event ID.
func WithIDGenerator(gen IDGenerator) SenderOption {
	return func(opts *senderOptions) {
		opts.idGenerator = gen
	}
}

// Sender implements eventbus.Sender by persisting events to an outbox store.
// It is designed to be used within a database transaction to ensure
// atomicity between business operations and event publishing.
type Sender struct {
	store   Store
	options senderOptions
}

// NewSender creates a new outbox Sender with the given store.
// The store should be transaction-scoped to ensure atomicity.
func NewSender(store Store, opts ...SenderOption) *Sender {
	options := defaultSenderOptions()

	for _, opt := range opts {
		opt(&options)
	}

	return &Sender{
		store:   store,
		options: options,
	}
}

// Send persists the event to the outbox store.
// This should be called within the same transaction as business operations.
func (s *Sender) Send(ctx context.Context, metadata *event.Metadata, data []byte) error {
	id, err := s.options.idGenerator(metadata)
	if err != nil {
		return err
	}

	msg := &Message{
		ID:         id,
		Metadata:   metadata,
		Data:       data,
		CreateTime: time.Now(),
	}

	return s.store.CreateOutboxMessage(ctx, msg)
}
