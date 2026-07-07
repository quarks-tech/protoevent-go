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

// GenerateV4 is a non-default IDGenerator. It ignores the metadata and mints a
// fresh random UUID v4 as the outbox row's unique primary key, independent of
// the caller-controlled Metadata.ID.
//
// Because the row ID is independent of Metadata.ID, the relay does NOT
// reconstruct the emitted CloudEvents id from Metadata.ID as it does under
// ReuseMetadataID — the id the publisher assigned is not persisted, so the
// relayed event's id will differ from the one the caller published. Use this
// only when the outbox row's identity does not need to match the event's
// identity.
func GenerateV4(_ *event.Metadata) (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}

	return id.String(), nil
}

// ReuseMetadataID is the default IDGenerator. It uses the event's Metadata.ID
// as the outbox row ID, giving the row and the event a single identity end to
// end: the relay persists this ID as the store's event_id column and
// reconstructs the emitted CloudEvents Metadata.ID from it, so the relayed
// event carries exactly the id the publisher assigned.
//
// Because CloudEvents ids are themselves UUIDs, this also scatters outbox
// primary-key inserts across TiDB Regions the same way a freshly minted UUID
// would (see storage ADR 012) — so the default gets hotspot avoidance and
// identity preservation together, with no tradeoff between them.
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
// Defaults to ReuseMetadataID. Use GenerateV4 to key rows on a freshly minted
// UUID independent of the event's Metadata.ID.
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
