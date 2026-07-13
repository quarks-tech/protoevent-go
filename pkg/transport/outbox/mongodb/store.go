// Package mongodb implements the outbox publish path and the change-stream relay
// contracts (stream.Store + relay.LeaderStore) over MongoDB.
package mongodb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

const (
	outboxCollection  = "outbox_messages"
	offsetsCollection = "outbox_offsets"
	lockCollection    = "relay_locks"

	// defaultRetention is the default outbox TTL; MUST exceed the oplog window
	// (see the README's resume-token-cliff sizing rule). Override with WithRetention.
	defaultRetention = 7 * 24 * time.Hour

	// indexOptionsConflictCode is the MongoDB server error code for
	// IndexOptionsConflict: an index with the same keys already exists with
	// different options (e.g. a different expireAfterSeconds).
	indexOptionsConflictCode = 85
)

// outboxDoc is one insert-only event envelope. Metadata is JSON bytes so the
// CloudEvents url.URL / Extensions map round-trip cleanly.
type outboxDoc struct {
	ID         string    `bson:"_id"`
	Metadata   []byte    `bson:"metadata"`
	Data       []byte    `bson:"data"`
	CreateTime time.Time `bson:"create_time"`
}

// offsetDoc is one consumer group's resume-token store. ResumeToken is stored
// as BSON binary (not an embedded document): the token is an opaque byte
// blob to this store, and only production resume tokens happen to also be
// valid BSON documents — storing as bson.Raw would reject arbitrary bytes.
type offsetDoc struct {
	Name        string    `bson:"_id"`
	ResumeToken []byte    `bson:"resume_token"`
	ClusterTime time.Time `bson:"cluster_time"`
	UpdateTime  time.Time `bson:"update_time"`
}

// Store realizes the outbox publish + relay-read contracts over a *mongo.Database.
type Store struct {
	db        *mongo.Database
	retention time.Duration
}

// Option configures a Store at construction time.
type Option func(*Store)

// WithRetention sets the outbox TTL applied by EnsureIndexes. Default 7 days;
// it MUST exceed the oplog window (see the README's resume-token-cliff sizing rule). A non-positive d is ignored
// (mirrors WithLogger/WithObserver's nil-guard style elsewhere in this
// codebase), leaving the default (or a prior option's value) intact.
//
// Operational caveat: MongoDB refuses to re-create an existing TTL index with
// a different expireAfterSeconds (IndexOptionsConflict) — changing retention
// on an existing collection requires a collMod on the index, not a restart
// with a new option value. EnsureIndexes surfaces that error with a hint.
func WithRetention(d time.Duration) Option {
	return func(s *Store) {
		if d > 0 {
			s.retention = d
		}
	}
}

// NewStore creates a Store realizing the outbox publish + relay-read contracts
// over db. Use WithRetention to tune the outbox TTL; it defaults to 7 days.
func NewStore(db *mongo.Database, opts ...Option) *Store {
	s := &Store{db: db, retention: defaultRetention}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

var (
	_ outbox.Store      = (*Store)(nil)
	_ relay.LeaderStore = (*Store)(nil)
)

// EnsureIndexes creates the TTL index on outbox.create_time. Idempotent for an
// unchanged retention; when the index already exists with a DIFFERENT
// expireAfterSeconds the server rejects the create (IndexOptionsConflict) and
// the error is surfaced with a collMod hint rather than masked — silently
// keeping the old TTL would defeat the point of changing WithRetention.
func (s *Store) EnsureIndexes(ctx context.Context) error {
	_, err := s.db.Collection(outboxCollection).Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "create_time", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(int32(s.retention / time.Second)),
	})
	if err != nil {
		var se mongo.ServerError
		if errors.As(err, &se) && se.HasErrorCode(indexOptionsConflictCode) {
			return fmt.Errorf("outbox: ensure ttl index: retention differs from the existing TTL index; "+
				"changing retention on an existing collection requires collMod on the create_time index, "+
				"not index re-creation: %w", err)
		}
		return fmt.Errorf("outbox: ensure ttl index: %w", err)
	}
	return nil
}

// CreateOutboxMessage inserts an unsequenced event envelope. Call on the
// session-bound ctx so it commits atomically with the business write.
//
// msg.ID must be non-empty (it is the row's _id; an empty _id would make the
// SECOND publish fail with a far-from-cause duplicate-key error). Unlike the
// TiDB store, which validates UUIDs, this store deliberately accepts any
// non-empty string: MongoDB's _id has no format requirement.
func (s *Store) CreateOutboxMessage(ctx context.Context, msg *outbox.Message) error {
	if msg.ID == "" {
		return fmt.Errorf("outbox: message ID is empty")
	}
	if msg.Metadata == nil {
		return fmt.Errorf("outbox: message metadata is nil")
	}
	meta, err := json.Marshal(msg.Metadata)
	if err != nil {
		return fmt.Errorf("outbox: marshal metadata: %w", err)
	}
	doc := outboxDoc{ID: msg.ID, Metadata: meta, Data: msg.Data, CreateTime: msg.CreateTime}
	if _, err := s.db.Collection(outboxCollection).InsertOne(ctx, doc); err != nil {
		return fmt.Errorf("outbox: insert: %w", err)
	}
	return nil
}

// LoadToken returns the consumer group's resume token ("" if none) as a string
// and the anchor clusterTime. The stored bytes are carried verbatim.
func (s *Store) LoadToken(ctx context.Context, name string) (string, time.Time, error) {
	var doc offsetDoc
	err := s.db.Collection(offsetsCollection).FindOne(ctx, bson.M{"_id": name}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("outbox: load token: %w", err)
	}
	return string(doc.ResumeToken), doc.ClusterTime, nil
}

// SaveToken upserts the consumer group's resume token + clusterTime. token is
// the opaque resume token as a string; it is stored as BSON binary bytes.
//
// token must be non-empty: LoadToken maps "" to "no stored position", so an
// empty-token save would ERASE the group's persisted position (the next Watch
// restarts "at now", silently skipping every event in between). The store
// rejects it with an error rather than trusting every caller to guard.
//
// The save is monotone in clusterTime (per the stream.Store contract): the
// filter only matches a stored row whose cluster_time <= the incoming one, so
// a stale leader finishing a slow window cannot rewind a newer position. When
// the stored row is newer, the filter matches nothing and the upsert attempts
// an insert, which hits the _id unique index — that duplicate-key error MEANS
// "stored token is newer" and the stale save is deliberately swallowed as a
// no-op. A stored row LACKING cluster_time (never written by this package;
// external tampering or manual repair) also matches, so a legacy/damaged row
// is healed by the next save instead of freezing the token forever behind the
// same swallowed duplicate-key path.
func (s *Store) SaveToken(ctx context.Context, name string, token string, clusterTime time.Time) error {
	if token == "" {
		return fmt.Errorf("outbox: save token: empty resume token for group %q; an empty token is never a valid position (it would erase the stored one)", name)
	}
	_, err := s.db.Collection(offsetsCollection).UpdateOne(ctx,
		bson.M{"_id": name, "$or": bson.A{
			bson.M{"cluster_time": bson.M{"$lte": clusterTime.UTC()}},
			bson.M{"cluster_time": bson.M{"$exists": false}},
		}},
		bson.M{"$set": bson.M{
			"resume_token": []byte(token),
			"cluster_time": clusterTime.UTC(),
			"update_time":  time.Now().UTC(),
		}},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil // stored row carries a newer clusterTime; stale save is a no-op
		}
		return fmt.Errorf("outbox: save token: %w", err)
	}
	return nil
}

// TryAcquireLeaderLock acquires or renews the lock via a conditional
// aggregation-pipeline upsert. Both the expiry decision and the new deadline
// use the SERVER clock ($$NOW): comparing against a client-side time.Now()
// would let a standby with a fast clock steal a live lease (dual leader) under
// clock skew between relay instances.
func (s *Store) TryAcquireLeaderLock(ctx context.Context, name, holderID string, ttl time.Duration) (bool, error) {
	// canTake: the caller already holds the lock (renewal) or the lease expired
	// per the server clock. On a fresh upsert both fields are missing; $ifNull
	// maps the missing expire_time to the epoch, which is < $$NOW, so the
	// pipeline claims the new document.
	canTake := bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "$eq", Value: bson.A{"$holder_id", holderID}}},
		bson.D{{Key: "$lt", Value: bson.A{
			bson.D{{Key: "$ifNull", Value: bson.A{"$expire_time", time.Unix(0, 0).UTC()}}},
			"$$NOW",
		}}},
	}}}
	// _id is intentionally not written by the pipeline: the filter already pins
	// it (and the upsert path copies it from the filter), so setting it too is
	// redundant (and would be fragile if it ever diverged). $$NOW is a Date;
	// expire_time stays a Date, with ttl added as milliseconds.
	// A sub-millisecond ttl truncates to 0ms — a lease born expired (silent
	// dual-leader churn) — so clamp to at least 1ms.
	millis := max(ttl.Milliseconds(), 1)
	update := mongo.Pipeline{
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "holder_id", Value: bson.D{{Key: "$cond", Value: bson.A{canTake, holderID, "$holder_id"}}}},
			{Key: "expire_time", Value: bson.D{{Key: "$cond", Value: bson.A{
				canTake,
				bson.D{{Key: "$add", Value: bson.A{"$$NOW", millis}}},
				"$expire_time",
			}}}},
		}}},
	}
	var doc struct {
		Holder string `bson:"holder_id"`
	}
	err := s.db.Collection(lockCollection).FindOneAndUpdate(ctx,
		bson.M{"_id": name},
		update,
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(&doc)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return false, nil // lost a concurrent upsert race; another instance holds the lock
		}
		return false, fmt.Errorf("outbox: acquire lock: %w", err)
	}
	// The pipeline always "succeeds"; whether we lead is decided by whose
	// holder_id survived the conditional write.
	return doc.Holder == holderID, nil
}

// ReleaseLeaderLock drops the lock if still held by holderID (graceful shutdown).
func (s *Store) ReleaseLeaderLock(ctx context.Context, name, holderID string) error {
	_, err := s.db.Collection(lockCollection).DeleteOne(ctx, bson.M{"_id": name, "holder_id": holderID})
	if err != nil {
		return fmt.Errorf("outbox: release lock: %w", err)
	}
	return nil
}

// decodeMessage rebuilds an outbox.Message from a stored outboxDoc, reversing
// CreateOutboxMessage's json.Marshal of the CloudEvents metadata. Its only
// caller is mongoStream.Next (watch.go).
func decodeMessage(doc outboxDoc) (*outbox.Message, error) {
	var md event.Metadata
	if err := json.Unmarshal(doc.Metadata, &md); err != nil {
		return nil, fmt.Errorf("outbox: unmarshal metadata: %w", err)
	}
	return &outbox.Message{
		ID:         doc.ID,
		Metadata:   &md,
		Data:       doc.Data,
		CreateTime: doc.CreateTime,
	}, nil
}
