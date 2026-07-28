package event_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

// FuzzContentSubtype asserts the parser's invariants against arbitrary input.
//
// The content type arrives from an incoming message, so this is attacker- and
// accident-reachable, and it is a codec-SELECTION function: a wrong answer routes a
// delivery to the wrong decoder or to none, which the subscriber turns into a parked
// delivery. Three separate defects were found here by hand (RFC-2045 parameters
// leaking into the codec name, case-sensitivity, a parameter returned as a subtype),
// which is exactly the profile fuzzing covers better than review does.
func FuzzContentSubtype(f *testing.F) {
	for _, seed := range []string{
		"", "application", "application/", "application/proto", "application/json",
		"application/cloudevents", "application/cloudevents+json",
		"application/cloudevents+json; charset=utf-8", "application/cloudevents;json",
		"application/cloudevents;charset=utf-8", "Application/PROTO", "applicationfoo",
		"text/plain", "application/;x=1", "application/cloudevents+",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, contentType string) {
		subtype, ok := event.ContentSubtype(contentType)
		if !ok {
			if subtype != "" {
				t.Fatalf("ContentSubtype(%q) = (%q, false): a rejected content type must yield no subtype", contentType, subtype)
			}

			return
		}

		// An accepted subtype is what gets handed to encoding.GetCodec, whose
		// registry stores lowercase names and does an exact lookup. Anything that
		// cannot be a codec name must not be reported as one.
		switch {
		case subtype == "":
			t.Fatalf("ContentSubtype(%q) = (\"\", true): empty subtype accepted", contentType)
		case strings.ContainsAny(subtype, ";="):
			t.Fatalf("ContentSubtype(%q) = %q: media-type parameter leaked into the codec name", contentType, subtype)
		case strings.TrimSpace(subtype) != subtype:
			t.Fatalf("ContentSubtype(%q) = %q: untrimmed codec name", contentType, subtype)
		case subtype != strings.ToLower(subtype):
			t.Fatalf("ContentSubtype(%q) = %q: codec name not lowercased (registry lookup is exact)", contentType, subtype)
		}
	})
}

// FuzzSplitType asserts that the event-type split never panics and never loses
// information. It guards the publisher's write-side gate, the subscriber's routing,
// and the RabbitMQ exchange/routing-key split — two of which used to panic on a
// dot-less type.
func FuzzSplitType(f *testing.F) {
	for _, seed := range []string{
		"", ".", "a", "a.b", "books.v1.BookCreated", ".leading", "trailing.",
		"..", "a..b", strings.Repeat("a.", 50) + "b",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, eventType string) {
		service, name, err := event.SplitType(eventType)
		if err != nil {
			return
		}

		switch {
		case service == "":
			t.Fatalf("SplitType(%q) accepted an empty service", eventType)
		case name == "":
			t.Fatalf("SplitType(%q) accepted an empty event name", eventType)
		case service+"."+name != eventType:
			t.Fatalf("SplitType(%q) = (%q, %q): the split is lossy", eventType, service, name)
		case strings.Contains(name, "."):
			t.Fatalf("SplitType(%q) = (%q, %q): split at the wrong dot (must be the last)", eventType, service, name)
		}
	})
}

// FuzzMetadataJSONRoundTrip asserts that the hand-written Metadata marshaler is
// stable: decoding a persisted envelope and re-encoding it must reach a fixed point.
//
// This is the invariant that the durable outbox row depends on. A non-idempotent
// round trip is how the *url.URL DataSchema corruption reached persisted rows, and
// how the later legacy-shape decoder could have rejected them.
func FuzzMetadataJSONRoundTrip(f *testing.F) {
	f.Add(`{"SpecVersion":"1.0","Type":"a.B","Source":"/s","ID":"1","Time":"2026-07-27T10:00:00Z","DataContentType":"application/proto"}`)
	f.Add(`{"SpecVersion":"1.0","Type":"a.B","ID":"1","DataSchema":"https://e.com/s.json"}`)
	f.Add(`{"SpecVersion":"1.0","Type":"a.B","ID":"1","DataSchema":{"Scheme":"https","User":{},"Host":"e.com","Path":"/s.json"}}`)
	f.Add(`{"SpecVersion":"1.0","Type":"a.B","ID":"1","DataSchema":null,"Extensions":{"k":"v"}}`)
	f.Add(`{}`)

	f.Fuzz(func(t *testing.T, raw string) {
		var first event.Metadata
		if err := json.Unmarshal([]byte(raw), &first); err != nil {
			return // not a decodable envelope; nothing to assert
		}

		encoded, err := json.Marshal(&first)
		if err != nil {
			// Only the documented DataSchema rejections may fail re-encoding: an
			// empty URI, or one that does not survive url.Parse. Both exist so that
			// an unreadable row is never written in the first place.
			if strings.Contains(err.Error(), "DataSchema") {
				return
			}
			t.Fatalf("re-marshaling a decoded envelope failed: %v (input %q)", err, raw)
		}

		var second event.Metadata
		if err := json.Unmarshal(encoded, &second); err != nil {
			t.Fatalf("re-decoding our own output failed: %v (output %q)", err, encoded)
		}

		reencoded, err := json.Marshal(&second)
		if err != nil {
			t.Fatalf("re-marshaling twice failed: %v", err)
		}
		if string(encoded) != string(reencoded) {
			t.Fatalf("round trip is not idempotent:\n first: %s\nsecond: %s", encoded, reencoded)
		}
	})
}
