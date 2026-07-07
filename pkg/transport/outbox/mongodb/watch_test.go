package mongodb

import (
	"errors"
	"fmt"
	"testing"

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
