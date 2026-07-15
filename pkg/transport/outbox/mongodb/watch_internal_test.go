package mongodb

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func TestIsHistoryLostByCode(t *testing.T) {
	err := mongo.CommandError{Code: changeStreamHistoryLostCode, Message: "resume of change stream was not possible"}
	if !isHistoryLost(err) {
		t.Fatalf("isHistoryLost(%v) = false, want true (code %d)", err, changeStreamHistoryLostCode)
	}
}

func TestIsHistoryLostByLabel(t *testing.T) {
	// Some server versions surface the condition via the label rather than (or
	// in addition to) the bare code.
	err := mongo.CommandError{Code: 1234, Labels: []string{nonResumableChangeStreamErrorLabel}}
	if !isHistoryLost(err) {
		t.Fatalf("isHistoryLost(%v) = false, want true (label %s)", err, nonResumableChangeStreamErrorLabel)
	}
}

func TestIsHistoryLostWrapped(t *testing.T) {
	// The stream surfaces errors via errors.Is/As-compatible wrapping; confirm
	// the classifier still finds the CommandError through fmt.Errorf's %w.
	err := fmt.Errorf("outbox: change stream: %w", mongo.CommandError{Code: changeStreamHistoryLostCode})
	if !isHistoryLost(err) {
		t.Fatalf("isHistoryLost(%v) = false, want true (wrapped)", err)
	}
}

func TestIsHistoryLostFalseForTransientError(t *testing.T) {
	// e.g. NetworkTimeout (89) — resumable, must NOT classify as history lost.
	err := mongo.CommandError{Code: 89, Message: "network timeout"}
	if isHistoryLost(err) {
		t.Fatalf("isHistoryLost(%v) = true, want false (transient/resumable error)", err)
	}
}

func TestIsHistoryLostFalseForNonServerError(t *testing.T) {
	if isHistoryLost(errors.New("boom")) {
		t.Fatal("isHistoryLost(plain error) = true, want false")
	}
	if isHistoryLost(nil) {
		t.Fatal("isHistoryLost(nil) = true, want false")
	}
}

// mustMarshalRaw builds a bson.Raw from any marshalable value for the
// decodeErrorFromRaw tests below.
func mustMarshalRaw(t *testing.T, v any) bson.Raw {
	t.Helper()
	b, err := bson.Marshal(v)
	if err != nil {
		t.Fatalf("bson.Marshal: %v", err)
	}
	return bson.Raw(b)
}

func TestDecodeErrorFromRawWellFormed(t *testing.T) {
	cause := errors.New("envelope decode failed")
	resumeDoc := bson.D{{Key: "_data", Value: "82ABCDEF"}}
	const clusterSecs = uint32(1_700_000_000)
	raw := mustMarshalRaw(t, bson.D{
		{Key: "_id", Value: resumeDoc},
		{Key: "operationType", Value: "insert"},
		{Key: "clusterTime", Value: bson.Timestamp{T: clusterSecs, I: 7}},
		{Key: "fullDocument", Value: bson.D{{Key: "_id", Value: "row-42"}}},
	})

	de := decodeErrorFromRaw(raw, cause)

	if !errors.Is(de, cause) {
		t.Fatalf("cause not carried: %v", de.Err)
	}
	wantToken := string(mustMarshalRaw(t, resumeDoc))
	if de.ResumeToken != wantToken {
		t.Fatalf("ResumeToken = %q, want the marshaled _id document %q", de.ResumeToken, wantToken)
	}
	wantCT := time.Unix(int64(clusterSecs), 0).UTC()
	if !de.ClusterTime.Equal(wantCT) {
		t.Fatalf("ClusterTime = %v, want %v", de.ClusterTime, wantCT)
	}
	if de.ID != "row-42" {
		t.Fatalf("ID = %q, want row-42", de.ID)
	}
}

func TestDecodeErrorFromRawWrongTypedID(t *testing.T) {
	// _id present but a string (not a document): the token must stay empty
	// while clusterTime and the row id are still extracted independently.
	cause := errors.New("boom")
	const clusterSecs = uint32(1_700_000_000)
	raw := mustMarshalRaw(t, bson.D{
		{Key: "_id", Value: "not-a-document"},
		{Key: "clusterTime", Value: bson.Timestamp{T: clusterSecs, I: 1}},
		{Key: "fullDocument", Value: bson.D{{Key: "_id", Value: "row-1"}}},
	})

	de := decodeErrorFromRaw(raw, cause)

	if de.ResumeToken != "" {
		t.Fatalf("ResumeToken = %q, want empty for a wrong-typed _id", de.ResumeToken)
	}
	if !de.ClusterTime.Equal(time.Unix(int64(clusterSecs), 0).UTC()) {
		t.Fatalf("ClusterTime = %v, want extracted despite bad _id", de.ClusterTime)
	}
	if de.ID != "row-1" {
		t.Fatalf("ID = %q, want row-1 despite bad _id", de.ID)
	}
}

func TestDecodeErrorFromRawMissingID(t *testing.T) {
	// No _id at all: same expectations as wrong-typed.
	cause := errors.New("boom")
	raw := mustMarshalRaw(t, bson.D{
		{Key: "clusterTime", Value: bson.Timestamp{T: 100, I: 0}},
		{Key: "fullDocument", Value: bson.D{{Key: "_id", Value: "row-2"}}},
	})

	de := decodeErrorFromRaw(raw, cause)

	if de.ResumeToken != "" {
		t.Fatalf("ResumeToken = %q, want empty when _id is missing", de.ResumeToken)
	}
	if de.ID != "row-2" {
		t.Fatalf("ID = %q, want row-2", de.ID)
	}
	if !de.ClusterTime.Equal(time.Unix(100, 0).UTC()) {
		t.Fatalf("ClusterTime = %v, want %v", de.ClusterTime, time.Unix(100, 0).UTC())
	}
}

func TestDecodeErrorFromRawMissingFullDocumentID(t *testing.T) {
	cause := errors.New("boom")
	resumeDoc := bson.D{{Key: "_data", Value: "82AA"}}
	raw := mustMarshalRaw(t, bson.D{
		{Key: "_id", Value: resumeDoc},
		{Key: "clusterTime", Value: bson.Timestamp{T: 200, I: 0}},
		{Key: "fullDocument", Value: bson.D{{Key: "other", Value: "field"}}},
	})

	de := decodeErrorFromRaw(raw, cause)

	if de.ID != "" {
		t.Fatalf("ID = %q, want empty when fullDocument._id is missing", de.ID)
	}
	if de.ResumeToken != string(mustMarshalRaw(t, resumeDoc)) {
		t.Fatalf("ResumeToken = %q, want extracted despite missing fullDocument._id", de.ResumeToken)
	}
}

func TestDecodeErrorFromRawNilAndGarbage(t *testing.T) {
	cause := errors.New("boom")
	for name, raw := range map[string]bson.Raw{
		"nil":     nil,
		"garbage": bson.Raw("\x03\x00\x00garbage-not-bson"),
	} {
		de := decodeErrorFromRaw(raw, cause) // must not panic
		if de == nil {
			t.Fatalf("%s: decodeErrorFromRaw returned nil", name)
		}
		if !errors.Is(de.Err, cause) {
			t.Fatalf("%s: cause not carried: %v", name, de.Err)
		}
		if de.ResumeToken != "" || de.ID != "" || !de.ClusterTime.IsZero() {
			t.Fatalf("%s: fields not empty: token=%q id=%q ct=%v", name, de.ResumeToken, de.ID, de.ClusterTime)
		}
	}
}
