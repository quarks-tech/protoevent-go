package mongodb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/stream"
)

var _ stream.StreamStore = (*Store)(nil)

// changeStreamHistoryLostCode is the MongoDB server error code for
// ChangeStreamHistoryLost: the resume token has fallen off the oplog window.
const changeStreamHistoryLostCode = 286

// nonResumableChangeStreamErrorLabel is the error label the server attaches to
// a non-resumable change-stream error (in addition to, or instead of, the bare
// code on some server versions).
const nonResumableChangeStreamErrorLabel = "NonResumableChangeStreamError"

// changeEvent is the decoded change-stream document (insert or invalidate).
type changeEvent struct {
	ID            bson.Raw       `bson:"_id"`           // resume token for THIS event
	OperationType string         `bson:"operationType"` // insert | invalidate | ...
	ClusterTime   bson.Timestamp `bson:"clusterTime"`
	FullDocument  outboxDoc      `bson:"fullDocument"` // present on insert
}

// Watch opens an insert-filtered change stream resumed from token (or "now").
func (s *Store) Watch(ctx context.Context, token string) (stream.Stream, error) {
	await := s.maxAwait
	if await <= 0 {
		await = time.Second
	}
	pipeline := mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.D{{Key: "operationType", Value: "insert"}}}},
	}
	opts := options.ChangeStream().SetMaxAwaitTime(await)
	if token != "" {
		opts = opts.SetResumeAfter(bson.Raw(token))
	}
	// else: no resumeAfter → the stream starts at "now" (v1 StartNow).

	cs, err := s.db.Collection(outboxCollection).Watch(ctx, pipeline, opts)
	if err != nil {
		return nil, fmt.Errorf("outbox: open change stream: %w", err)
	}
	return &mongoStream{cs: cs}, nil
}

// mongoStream adapts a *mongo.ChangeStream to stream.Stream.
type mongoStream struct {
	cs *mongo.ChangeStream
}

// Next drains one event if buffered; on an empty batch it blocks up to
// maxAwaitTime and returns (nil,false,nil). A stream error → (nil,false,err),
// classified to stream.ErrHistoryLost when the resume token fell off the oplog.
func (m *mongoStream) Next(ctx context.Context) (*stream.Event, bool, error) {
	// TryNext returns false when the current batch is drained WITHOUT closing
	// the stream (respecting maxAwaitTime) — exactly the window semantics we want.
	if !m.cs.TryNext(ctx) {
		if err := m.cs.Err(); err != nil {
			if isHistoryLost(err) {
				return nil, false, stream.ErrHistoryLost
			}
			return nil, false, err
		}
		return nil, false, nil // empty window
	}
	var ce changeEvent
	if err := m.cs.Decode(&ce); err != nil {
		return nil, false, fmt.Errorf("outbox: decode change event: %w", err)
	}
	if ce.OperationType == "invalidate" {
		return &stream.Event{Invalidate: true}, true, nil
	}
	msg, err := decodeMessage(ce.FullDocument)
	if err != nil {
		return nil, false, err
	}
	return &stream.Event{
		Message:     msg,
		ResumeToken: string(ce.ID),
		ClusterTime: time.Unix(int64(ce.ClusterTime.T), 0).UTC(),
	}, true, nil
}

// PBRT returns the postBatchResumeToken after an empty window. The driver
// surfaces the batch-level token via ResumeToken(); the token's embedded
// clusterTime is not exposed, so we stamp "now" as the anchor (a connected,
// caught-up consumer's head is approximately now).
func (m *mongoStream) PBRT() (string, time.Time) {
	tok := m.cs.ResumeToken()
	if tok == nil {
		return "", time.Time{}
	}
	return string(tok), time.Now().UTC()
}

func (m *mongoStream) Close(ctx context.Context) error { return m.cs.Close(ctx) }

// isHistoryLost reports whether err is the server's ChangeStreamHistoryLost
// error (code 286), signaled either by the bare code or by the
// NonResumableChangeStreamError label the server attaches to it.
func isHistoryLost(err error) bool {
	var se mongo.ServerError
	if !errors.As(err, &se) {
		return false
	}
	return se.HasErrorCode(changeStreamHistoryLostCode) || se.HasErrorLabel(nonResumableChangeStreamErrorLabel)
}
