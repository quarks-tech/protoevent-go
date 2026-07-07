package outbox_test

import (
	"testing"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// A drained message must expose its assigned log offset.
func TestMessageHasSeqField(t *testing.T) {
	m := outbox.Message{Seq: 42}
	if m.Seq != 42 {
		t.Fatalf("Seq = %d, want 42", m.Seq)
	}
}
