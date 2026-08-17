package outbox_test

import (
	"strings"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// validMetadata is the baseline every case below mutates one field of, so a
// rejection can only come from the rule under test.
func validMetadata() *event.Metadata {
	md := event.NewMetadata("svc.Event")
	md.ID = "id-1"
	md.Time = time.Now().UTC()

	return md
}

// TestValidateMetadataEnforcesTheSendableShape pins the write-side rules that
// keep a row the relay can neither send nor classify out of the store.
//
// These are the SAME rules eventbus.publish enforces, and they have to be
// repeated on this path because the outbox Sender is reachable without the bus:
// a forwarder re-publishing an incoming event.Metadata straight through
// Sender.Send never passes through publish at all. What the omission cost was not
// a failed publish but a wedged relay — the sender needs the dot to split
// exchange from routing key, the AMQP field encoder refuses a nested extension,
// both at SEND time, after the row committed with the caller's business
// transaction.
func TestValidateMetadataEnforcesTheSendableShape(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*event.Metadata)
		wantErr string
	}{
		{
			name:   "valid metadata passes",
			mutate: func(*event.Metadata) {},
		},
		{
			name:    "dot-less event type",
			mutate:  func(md *event.Metadata) { md.Type = "BookCreated" },
			wantErr: "malformed event type",
		},
		{
			name:    "empty event type",
			mutate:  func(md *event.Metadata) { md.Type = "" },
			wantErr: "malformed event type",
		},
		{
			name:    "extension colliding with a core attribute",
			mutate:  func(md *event.Metadata) { md.Extensions = map[string]any{"source": "audit"} },
			wantErr: "collides with a core CloudEvents attribute",
		},
		{
			name:    "extension using the binary-mode namespace",
			mutate:  func(md *event.Metadata) { md.Extensions = map[string]any{"cloudEvents:id": "x"} },
			wantErr: "collides with a core CloudEvents attribute",
		},
		{
			name:    "nested extension value",
			mutate:  func(md *event.Metadata) { md.Extensions = map[string]any{"ctx": map[string]any{"a": 1}} },
			wantErr: "is not a CloudEvents extension value",
		},
		{
			name:   "ordinary extension is untouched",
			mutate: func(md *event.Metadata) { md.Extensions = map[string]any{"tenant": "acme"} },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			md := validMetadata()
			tc.mutate(md)

			err := outbox.ValidateMetadata(md)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateMetadata = %v, want nil", err)
				}

				return
			}
			if err == nil {
				t.Fatalf("ValidateMetadata = nil, want an error containing %q "+
					"(the row would commit and then wedge the relay lane)", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateMetadata = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}
