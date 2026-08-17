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

// TestEnvelopeRoundTripLosesLargeIntegerExtensions is the evidence for the
// validation rule above: it demonstrates the corruption on the real
// Marshal/Unmarshal pair rather than asserting it from the JSON spec.
//
// event.ValidExtensionValue deliberately accepts int64 at ANY magnitude, and it is
// right to: the AMQP encoder writes int64 as a 64-bit field ('l'), so nothing is
// lost on the wire it was calibrated for. The outbox envelope is a NARROWER
// channel — encoding/json unmarshals every JSON number into float64, whose mantissa
// is 53 bits — so the same value that survives a direct publish is durably
// corrupted by a round trip through an outbox row. That asymmetry is why the
// rejection belongs here and not in pkg/event.
func TestEnvelopeRoundTripLosesLargeIntegerExtensions(t *testing.T) {
	const beyondFloat64 = int64(1)<<53 + 1

	md := validMetadata()
	md.Extensions = map[string]any{"account": beyondFloat64}

	b, err := outbox.MarshalMetadata(md)
	if err != nil {
		t.Fatalf("MarshalMetadata: %v", err)
	}
	got, err := outbox.UnmarshalMetadata(b)
	if err != nil {
		t.Fatalf("UnmarshalMetadata: %v", err)
	}

	// Not merely a type change: the VALUE differs. int64(1)<<53+1 has no float64
	// representation, so it comes back as int64(1)<<53 — off by one, silently, in
	// a durable row.
	back, ok := got.Extensions["account"].(float64)
	if !ok {
		t.Fatalf("extension came back as %T; this test is documenting the float64 round trip",
			got.Extensions["account"])
	}
	if int64(back) == beyondFloat64 {
		t.Fatalf("int64(%d) survived the envelope round trip; if the encoding now preserves "+
			"exact integers, the ValidateMetadata rejection above is obsolete and should go", beyondFloat64)
	}
}

// TestEnvelopeRoundTripCoercesBinaryAndTimestampExtensions pins the two coercions
// that are NOT corruption, so they are a known property rather than an accident.
//
// []byte and time.Time are legal CloudEvents extension types and their VALUES
// survive (base64, RFC3339). Only the Go type changes on the way back, which
// matters solely to a caller that type-asserts. They are therefore documented
// rather than rejected — rejecting them would drop two legitimate CloudEvents
// types for a fidelity issue that loses no data.
func TestEnvelopeRoundTripCoercesBinaryAndTimestampExtensions(t *testing.T) {
	stamp := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	md := validMetadata()
	md.Extensions = map[string]any{"blob": []byte("hi"), "seen": stamp}

	b, err := outbox.MarshalMetadata(md)
	if err != nil {
		t.Fatalf("MarshalMetadata: %v", err)
	}
	got, err := outbox.UnmarshalMetadata(b)
	if err != nil {
		t.Fatalf("UnmarshalMetadata: %v", err)
	}

	if blob, ok := got.Extensions["blob"].(string); !ok || blob != "aGk=" {
		t.Fatalf(`extension "blob" = %#v, want the base64 string "aGk="`, got.Extensions["blob"])
	}
	if seen, ok := got.Extensions["seen"].(string); !ok || seen != stamp.Format(time.RFC3339Nano) {
		t.Fatalf(`extension "seen" = %#v, want the RFC3339 string %q`,
			got.Extensions["seen"], stamp.Format(time.RFC3339Nano))
	}
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
		{
			name:    "int64 extension beyond JSON's exact-integer range",
			mutate:  func(md *event.Metadata) { md.Extensions = map[string]any{"account": int64(1)<<53 + 1} },
			wantErr: "cannot survive the outbox envelope",
		},
		{
			name:    "negative int64 extension beyond JSON's exact-integer range",
			mutate:  func(md *event.Metadata) { md.Extensions = map[string]any{"account": -(int64(1)<<53 + 1)} },
			wantErr: "cannot survive the outbox envelope",
		},
		{
			name:   "int64 extension inside JSON's exact-integer range passes",
			mutate: func(md *event.Metadata) { md.Extensions = map[string]any{"account": int64(1) << 53} },
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
