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

var _ stream.Store = (*Store)(nil)

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
// maxAwait becomes the change stream's server-side maxAwaitTime — the relay
// passes its DrainWindow, keeping a single latency knob. A non-positive
// maxAwait leaves the driver/server default in place (the relay validates
// DrainWindow > 0, so this is unreachable through the stream relay).
func (s *Store) Watch(ctx context.Context, token string, maxAwait time.Duration) (stream.Stream, error) {
	// invalidate MUST pass the filter too: a $match on "insert" alone silently
	// drops the collection-dropped/renamed invalidate event, leaving the
	// stream reporting empty windows forever instead of surfacing the fatal
	// ErrStreamInvalidated via Next below (verified live: see
	// TestWatchSurfacesInvalidateOnDrop).
	pipeline := mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.D{
			{Key: "operationType", Value: bson.D{{Key: "$in", Value: bson.A{"insert", "invalidate"}}}},
		}}},
	}
	opts := options.ChangeStream()
	if maxAwait > 0 {
		opts = opts.SetMaxAwaitTime(maxAwait)
	}
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
// classified to stream.ErrHistoryLost when the resume token fell off the
// oplog, stream.ErrStreamInvalidated on an invalidate event (collection
// drop/rename), and *stream.DecodeError when THIS event's payload fails to
// decode — the DecodeError carries the event's resume position so the relay
// can park it and resume past it, instead of reopening onto the same poison
// event forever.
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
		// The envelope itself failed to decode; recover the resume position
		// from the raw change document so the caller can still skip past it.
		return nil, false, decodeErrorFromRaw(m.cs.Current, err)
	}
	if ce.OperationType == "invalidate" {
		return nil, false, stream.ErrStreamInvalidated
	}
	msg, err := decodeMessage(ce.FullDocument)
	if err != nil {
		return nil, false, &stream.DecodeError{
			ID:          ce.FullDocument.ID,
			ResumeToken: string(ce.ID),
			ClusterTime: time.Unix(int64(ce.ClusterTime.T), 0).UTC(),
			Err:         err,
		}
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
// caught-up consumer's head is approximately now). The stamp is truncated to
// seconds to match the granularity of event clusterTimes (bson.Timestamp
// carries whole seconds): a millisecond-precision "now" would compare as
// NEWER than an event committed in the same second, making SaveToken's
// monotone guard misclassify the very next event-token save as stale.
func (m *mongoStream) PBRT() (string, time.Time) {
	tok := m.cs.ResumeToken()
	if tok == nil {
		return "", time.Time{}
	}
	return string(tok), time.Now().UTC().Truncate(time.Second)
}

func (m *mongoStream) Close(ctx context.Context) error { return m.cs.Close(ctx) }

// decodeErrorFromRaw builds a *stream.DecodeError from the raw change
// document when the changeEvent envelope itself fails to decode. Each field
// is best-effort: the resume token (_id), clusterTime, and the outbox row id
// (fullDocument._id) are extracted independently, so whatever survives the
// corruption still positions the caller.
func decodeErrorFromRaw(raw bson.Raw, err error) *stream.DecodeError {
	de := &stream.DecodeError{Err: err}
	if v, lerr := raw.LookupErr("_id"); lerr == nil {
		if doc, ok := v.DocumentOK(); ok {
			de.ResumeToken = string(doc)
		}
	}
	if v, lerr := raw.LookupErr("clusterTime"); lerr == nil {
		if t, _, ok := v.TimestampOK(); ok {
			de.ClusterTime = time.Unix(int64(t), 0).UTC()
		}
	}
	if v, lerr := raw.LookupErr("fullDocument", "_id"); lerr == nil {
		if s, ok := v.StringValueOK(); ok {
			de.ID = s
		}
	}
	return de
}

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
