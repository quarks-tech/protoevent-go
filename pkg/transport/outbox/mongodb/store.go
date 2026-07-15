// Package mongodb implements the outbox publish path and the change-stream relay
// contracts (stream.Store + relay.LeaderStore) over MongoDB.
package mongodb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// The three collections of one outbox instance. One prefix applies to all of
// them as a unit (WithCollectionPrefix): an outbox instance IS the
// three-collection set — prefixing them together gives each instance its own
// log, resume tokens, and leader locks, so several independent outboxes can
// coexist in one database. (Unlike TiDB, a separate *mongo.Database per
// outbox also works — session transactions span databases within a cluster —
// so the prefix is a convenience, not the only path.)
const (
	baseOutboxCollection  = "outbox_messages"
	baseOffsetsCollection = "outbox_offsets"
	baseLockCollection    = "relay_locks"

	// defaultRetention is the default outbox TTL; MUST exceed the oplog window
	// (see the README's resume-token-cliff sizing rule). Override with WithRetention.
	defaultRetention = 7 * 24 * time.Hour

	// indexOptionsConflictCode is the MongoDB server error code for
	// IndexOptionsConflict: an index with the same keys already exists with
	// different options (e.g. a different expireAfterSeconds).
	indexOptionsConflictCode = 85
)

// collectionPrefixPattern mirrors the TiDB store's table-prefix rule: keep
// the prefix a conservative identifier fragment even though MongoDB itself
// allows more, so one prefix value works verbatim across both backends.
var collectionPrefixPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

const maxCollectionPrefixLen = 40

// config carries construction-time configuration for NewStore. Option is
// func(*config), not func(*Store): the underlying type of an exported func
// type is API, and binding options to the concrete Store would weld the
// option set to that one type forever (the tidb module uses the same shape).
type config struct {
	prefix    string
	retention time.Duration
}

// WithCollectionPrefix names this outbox instance: all three collections get
// the prefix, letting several independent outboxes coexist in one database,
// each with its own commit order, retention, and consumer groups.
//
// An invalid prefix panics: it is static developer configuration, not runtime
// input — the regexp.MustCompile convention for programmer error.
func WithCollectionPrefix(prefix string) Option {
	if prefix == "" || len(prefix) > maxCollectionPrefixLen || !collectionPrefixPattern.MatchString(prefix) {
		panic(fmt.Errorf("outbox: collection prefix %q is not a valid identifier fragment (want %s, at most %d chars)",
			prefix, collectionPrefixPattern, maxCollectionPrefixLen))
	}
	return func(c *config) { c.prefix = prefix }
}

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
	CommitTime  time.Time `bson:"cluster_time"`
	UpdateTime  time.Time `bson:"update_time"`
}

// Store realizes the outbox publish + relay-read contracts over a *mongo.Database.
type Store struct {
	db        *mongo.Database
	retention time.Duration

	collMessages string
	collOffsets  string
	collLocks    string
}

// Option configures a Store at construction time.
type Option func(*config)

// WithRetention sets the outbox TTL applied by EnsureIndexes — which MUST be
// called for the TTL to exist at all; the option alone changes nothing.
// Default 7 days; it MUST exceed the oplog window (see the README's
// resume-token-cliff sizing rule). The value is stored as given — a
// non-positive or fractional-second d is rejected loudly by EnsureIndexes
// rather than silently ignored here: a zero from a miscomputed config field
// is a caller bug, not a request for the default (unlike a nil
// Logger/Observer, which has exactly one sensible meaning).
//
// Retention here is a TTL index, pruning independently of delivery; the TiDB
// runtime's sequence.WithRetention is a delivery-gated sweep instead — same
// knob name, deliberately different mechanism per backend.
//
// CLOCK CAVEAT: the TTL anchor is create_time, stamped by the PUBLISHER's
// clock at Send (MongoDB inserts have no server-side default). Publisher
// clock sync is therefore a retention input: a clock far ahead pins rows past
// the window; a clock behind by more than the retention lets the TTL monitor
// reap rows early. Early reaping cannot lose undelivered events in normal
// operation (the change stream reads the OPLOG — a TTL delete does not
// remove the insert event a resuming relay replays), but it CAN shrink the
// break-glass re-read window after ErrHistoryLost, which reads the
// collection. Keep publisher clocks NTP-synced and the window comfortably
// above the oplog window.
//
// Operational caveat: MongoDB refuses to re-create an existing TTL index with
// a different expireAfterSeconds (IndexOptionsConflict) — changing retention
// on an existing collection requires a collMod on the index, not a restart
// with a new option value. EnsureIndexes surfaces that error with a hint.
func WithRetention(d time.Duration) Option {
	return func(c *config) { c.retention = d }
}

// NewStore creates a Store realizing the outbox publish + relay-read contracts
// over db. Use WithRetention to tune the outbox TTL (default 7 days) and
// WithCollectionPrefix to run several independent outbox instances in one
// database.
func NewStore(db *mongo.Database, opts ...Option) *Store {
	c := config{retention: defaultRetention}
	for _, opt := range opts {
		opt(&c)
	}
	return &Store{
		db:           db,
		retention:    c.retention,
		collMessages: c.prefix + baseOutboxCollection,
		collOffsets:  c.prefix + baseOffsetsCollection,
		collLocks:    c.prefix + baseLockCollection,
	}
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
//
// The retention is validated here rather than in WithRetention so the failure
// is loud: expireAfterSeconds is an int32 of whole seconds, and a sub-second
// retention would truncate to 0 — a TTL that expires every outbox row as soon
// as create_time passes, silently losing events the relay hasn't drained.
// Fractional seconds are rejected outright rather than silently truncated
// (1500ms would otherwise become a 1s TTL that fires earlier than configured).
func (s *Store) EnsureIndexes(ctx context.Context) error {
	if s.retention%time.Second != 0 {
		return fmt.Errorf("outbox: ensure ttl index: retention %v is not a whole number of seconds (expireAfterSeconds is int32 seconds; truncating would expire rows earlier than configured)", s.retention)
	}
	secs := int64(s.retention / time.Second)
	if secs < 1 || secs > math.MaxInt32 {
		return fmt.Errorf("outbox: ensure ttl index: retention %v outside the TTL range [1s, ~68y]", s.retention)
	}
	_, err := s.db.Collection(s.collMessages).Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "create_time", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(int32(secs)),
	})
	if err != nil {
		if se, ok := errors.AsType[mongo.ServerError](err); ok && se.HasErrorCode(indexOptionsConflictCode) {
			return fmt.Errorf("outbox: ensure ttl index: retention differs from the existing TTL index; "+
				"changing retention on an existing collection requires collMod on the create_time index, "+
				"not index re-creation: %w", err)
		}
		return fmt.Errorf("outbox: ensure ttl index: %w", err)
	}
	return nil
}

// CreateOutboxMessage inserts an unsequenced event envelope. ctx MUST carry a
// session with a running transaction (mongo.Session.WithTransaction binds it):
// an outbox row that commits independently of the business write is a phantom
// event, so a transactionless ctx is rejected loudly — the same fail-loud
// stance the TiDB store takes for autocommit publishes.
//
// msg.ID must be non-empty (it is the row's _id; an empty _id would make the
// SECOND publish fail with a far-from-cause duplicate-key error). Unlike the
// TiDB store, which validates UUIDs, this store deliberately accepts any
// non-empty string: MongoDB's _id has no format requirement.
//
// msg.CreateTime is persisted as stamped (the publisher's clock): it anchors
// the TTL index, so publisher clock sync matters — see WithRetention's clock
// caveat. (The TiDB store DB-stamps create_time instead; MongoDB inserts
// have no server-side default, and the insert-only + duplicate-key contract
// rules out the upsert tricks that could fetch one.)
func (s *Store) CreateOutboxMessage(ctx context.Context, msg *outbox.Message) error {
	if msg.ID == "" {
		return errors.New("outbox: message ID is empty")
	}
	if msg.Metadata == nil {
		return errors.New("outbox: message metadata is nil")
	}
	if msg.CreateTime.IsZero() {
		// create_time is the TTL anchor: a zero value (0001-01-01) is already
		// past every retention window, so the TTL monitor would silently reap
		// the row on its next pass — an event lost before the relay drains it.
		return errors.New("outbox: message create time is zero; set Message.CreateTime before publishing")
	}
	if sess := mongo.SessionFromContext(ctx); sess == nil || !sess.TransactionRunning() {
		return errors.New("outbox: create message: ctx has no running transaction; " +
			"publish must share the business write's session (see the README's publishing example)")
	}
	meta, err := json.Marshal(msg.Metadata)
	if err != nil {
		return fmt.Errorf("outbox: marshal metadata: %w", err)
	}
	doc := outboxDoc{ID: msg.ID, Metadata: meta, Data: msg.Data, CreateTime: msg.CreateTime}
	if _, err := s.db.Collection(s.collMessages).InsertOne(ctx, doc); err != nil {
		return fmt.Errorf("outbox: insert: %w", err)
	}
	return nil
}

// LoadToken returns the consumer group's resume token ("" if none) as a string
// and the anchor clusterTime. The stored bytes are carried verbatim.
func (s *Store) LoadToken(ctx context.Context, name string) (string, time.Time, error) {
	var doc offsetDoc
	err := s.db.Collection(s.collOffsets).FindOne(ctx, bson.M{"_id": name}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("outbox: load token: %w", err)
	}
	return string(doc.ResumeToken), doc.CommitTime, nil
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
// "stored token is newer" and the stale save is skipped, reported as
// persisted=false so the caller does not advance its local trackers past a
// position that was never stored. A stored row LACKING cluster_time (never
// written by this package; external tampering or manual repair) also matches,
// so a legacy/damaged row is healed by the next save instead of freezing the
// token forever behind the same duplicate-key path.
func (s *Store) SaveToken(ctx context.Context, name string, token string, clusterTime time.Time) (bool, error) {
	if token == "" {
		return false, fmt.Errorf("outbox: save token: empty resume token for group %q; an empty token is never a valid position (it would erase the stored one)", name)
	}
	_, err := s.db.Collection(s.collOffsets).UpdateOne(ctx,
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
			return false, nil // stored row carries a newer clusterTime; stale save skipped
		}
		return false, fmt.Errorf("outbox: save token: %w", err)
	}
	return true, nil
}

// DeleteToken removes the consumer group's stored resume token. It is the
// reset step of the ErrHistoryLost / ErrInvalidated break-glass procedures
// (see the README's runbook: after a token falls off the oplog or the
// collection is invalidated, the stored position is unusable — deleting it
// makes the next Watch start at "now", after which the runbook's re-read
// covers the gap) and the decommissioning step for a retired consumer group
// (a retired group's token row otherwise alerts on committed-token age
// forever). Deleting a missing token is a no-op.
func (s *Store) DeleteToken(ctx context.Context, name string) error {
	if _, err := s.db.Collection(s.collOffsets).DeleteOne(ctx, bson.M{"_id": name}); err != nil {
		return fmt.Errorf("outbox: delete token: %w", err)
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
	err := s.db.Collection(s.collLocks).FindOneAndUpdate(ctx,
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
	_, err := s.db.Collection(s.collLocks).DeleteOne(ctx, bson.M{"_id": name, "holder_id": holderID})
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
