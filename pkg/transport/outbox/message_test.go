package outbox_test

import (
	"testing"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// API-surface smoke test: Message must keep exposing the assigned log offset
// (renaming/removing Seq is a breaking change for relay consumers). The
// actual relay-side assignment is covered by the tidb store tests.
func TestMessageSeqFieldSmoke(t *testing.T) {
	m := outbox.Message{Seq: 42}
	if m.Seq != 42 {
		t.Fatalf("Seq = %d, want 42", m.Seq)
	}
}
