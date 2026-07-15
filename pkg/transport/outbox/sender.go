package outbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

// RowIDGenerator produces the outbox row ID for a message. It receives the event
// metadata so a generator may derive the ID from it — in particular,
// ReuseMetadataID returns the event's own ID.
//
// Note: the TiDB store requires UUID IDs (event_id is a BINARY(16) column); the
// MongoDB store accepts any string. A custom RowIDGenerator used with the TiDB
// backend must emit UUID strings.
type RowIDGenerator func(md *event.Metadata) (string, error)

// GenerateUUIDv4 is a non-default RowIDGenerator. It ignores the metadata and mints a
// fresh random UUID v4 as the outbox row's unique primary key, independent of
// the caller-controlled Metadata.ID.
//
// This only decouples the row key from the event's identity: the full
// event.Metadata (including the publisher-assigned ID) is persisted in the
// row's metadata document and relayed verbatim, so the relayed event still
// carries exactly the id the caller published. Use GenerateUUIDv4 when the store's
// row key must not depend on caller-controlled input; consumer-side
// Metadata.ID dedup keeps working either way.
func GenerateUUIDv4(_ *event.Metadata) (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("outbox: generate uuid v4 row id: %w", err)
	}

	return id.String(), nil
}

// ReuseMetadataID is the default RowIDGenerator. It copies the event's
// Metadata.ID into the outbox row ID, giving the row and the event a single
// identity end to end: the store's event_id column equals the published
// Metadata.ID. (The relayed event's id always comes from the persisted
// metadata document, under any generator; this default additionally makes the
// row key match it.)
//
// Because CloudEvents ids are themselves UUIDs, this also scatters outbox
// primary-key inserts across TiDB Regions the same way a freshly minted UUID
// would (see storage ADR 012) — so the default gets hotspot avoidance and
// identity preservation together, with no tradeoff between them.
//
// Delivery order is the log order (assigned seq for the TiDB runtime, oplog
// commit order for the MongoDB runtime), never the row ID, so the ID format
// does not affect ordering. The only requirement is that Metadata.ID be
// unique (it is the row's primary key).
func ReuseMetadataID(md *event.Metadata) (string, error) {
	return md.ID, nil
}

type senderOptions struct {
	idGenerator RowIDGenerator
}

func defaultSenderOptions() senderOptions {
	return senderOptions{
		idGenerator: ReuseMetadataID,
	}
}

// SenderOption configures an outbox Sender.
type SenderOption func(opts *senderOptions)

// WithRowIDGenerator sets the generator used to produce the outbox row ID.
// Defaults to ReuseMetadataID. Use GenerateUUIDv4 to key rows on a freshly minted
// UUID independent of the event's Metadata.ID. A nil gen is ignored, keeping
// the default (the With* nil-guard convention — installing nil would only
// mint a panic at the first Send, inside the caller's business transaction).
//
// Note: the TiDB store requires UUID IDs; the MongoDB store accepts any
// string. A custom RowIDGenerator used with the TiDB backend must emit UUIDs.
func WithRowIDGenerator(gen RowIDGenerator) SenderOption {
	return func(opts *senderOptions) {
		if gen != nil {
			opts.idGenerator = gen
		}
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
	if metadata == nil {
		return errors.New("outbox: metadata must not be nil")
	}

	id, err := s.options.idGenerator(metadata)
	if err != nil {
		return err
	}

	msg := &Message{
		ID:         id,
		Metadata:   metadata,
		Data:       data,
		CreateTime: time.Now().UTC(),
	}

	return s.store.CreateOutboxMessage(ctx, msg)
}
