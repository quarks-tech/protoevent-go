// Package mongodb implements the outbox publish path and the change-stream relay
// contracts (stream.StreamStore + relay.LeaderStore) over MongoDB.
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

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

const (
	outboxCollection  = "outbox"
	offsetsCollection = "outbox_offsets"
	lockCollection    = "relay_lock"

	// retentionDays is the outbox TTL; MUST exceed the oplog window (design §7).
	retentionDays = 7
)

// outboxDoc is one insert-only event envelope. Metadata is JSON bytes so the
// CloudEvents url.URL / Extensions map round-trip cleanly.
type outboxDoc struct {
	ID         string    `bson:"_id"`
	Metadata   []byte    `bson:"metadata"`
	Data       []byte    `bson:"data"`
	CreateTime time.Time `bson:"create_time"`
}

// offsetDoc is one consumer group's resume-token store.
type offsetDoc struct {
	Name        string    `bson:"_id"`
	ResumeToken bson.Raw  `bson:"resume_token"`
	ClusterTime time.Time `bson:"cluster_time"`
	UpdateTime  time.Time `bson:"update_time"`
}

type lockDoc struct {
	Name       string    `bson:"_id"`
	HolderID   string    `bson:"holder_id"`
	ExpireTime time.Time `bson:"expire_time"`
}

// Store realizes the outbox publish + relay-read contracts over a *mongo.Database.
type Store struct {
	db *mongo.Database
}

func NewStore(db *mongo.Database) *Store { return &Store{db: db} }

var (
	_ outbox.Store      = (*Store)(nil)
	_ relay.LeaderStore = (*Store)(nil)
)

// EnsureIndexes creates the TTL index on outbox.create_time. Idempotent.
func (s *Store) EnsureIndexes(ctx context.Context) error {
	_, err := s.db.Collection(outboxCollection).Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "create_time", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(int32(retentionDays * 24 * 60 * 60)),
	})
	if err != nil {
		return fmt.Errorf("outbox: ensure ttl index: %w", err)
	}
	return nil
}

// CreateOutboxMessage inserts an unsequenced event envelope. Call on the
// session-bound ctx so it commits atomically with the business write.
func (s *Store) CreateOutboxMessage(ctx context.Context, msg *outbox.Message) error {
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
// and the anchor clusterTime. The stored bson.Raw bytes are carried verbatim.
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
// the opaque resume token as a string; it is stored as bson.Raw bytes.
func (s *Store) SaveToken(ctx context.Context, name string, token string, clusterTime time.Time) error {
	_, err := s.db.Collection(offsetsCollection).UpdateOne(ctx,
		bson.M{"_id": name},
		bson.M{"$set": bson.M{
			"resume_token": bson.Raw(token),
			"cluster_time": clusterTime.UTC(),
			"update_time":  time.Now().UTC(),
		}},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("outbox: save token: %w", err)
	}
	return nil
}

// TryAcquireLeaderLock acquires or renews the lock via a conditional upsert.
func (s *Store) TryAcquireLeaderLock(ctx context.Context, name, holderID string, ttl time.Duration) (bool, error) {
	now := time.Now()
	filter := bson.M{
		"_id": name,
		"$or": bson.A{
			bson.M{"expire_time": bson.M{"$lt": now}},
			bson.M{"holder_id": holderID},
		},
	}
	update := bson.M{"$set": lockDoc{Name: name, HolderID: holderID, ExpireTime: now.Add(ttl)}}
	res, err := s.db.Collection(lockCollection).UpdateOne(ctx, filter, update, options.UpdateOne().SetUpsert(true))
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return false, nil // another instance holds a live lock
		}
		return false, fmt.Errorf("outbox: acquire lock: %w", err)
	}
	return res.MatchedCount > 0 || res.UpsertedCount > 0, nil
}

// ReleaseLeaderLock drops the lock if still held by holderID (graceful shutdown).
func (s *Store) ReleaseLeaderLock(ctx context.Context, name, holderID string) error {
	_, err := s.db.Collection(lockCollection).DeleteOne(ctx, bson.M{"_id": name, "holder_id": holderID})
	if err != nil {
		return fmt.Errorf("outbox: release lock: %w", err)
	}
	return nil
}

// decodeMessage, which rebuilds an outbox.Message from a stored outboxDoc,
// lands in Task 4 alongside Watch (its only caller) — golangci-lint's
// `unused` linter flags a free function with no caller, and the repo
// carries zero //nolint directives.
