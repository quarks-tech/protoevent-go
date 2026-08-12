package message

import "testing"

// TestIsStructured pins content-mode dispatch against RFC 2045 spellings.
//
// A structured-mode delivery routed to the binary unmarshaler finds no
// cloudEvents:specversion header, fails as an unprocessable event, and is parked
// in the DLQ — so every event from a publisher that appends a charset parameter
// disappears from the handler's point of view. Byte-exact matching against
// "application/cloudevents+json" does exactly that.
func TestIsStructured(t *testing.T) {
	structured := []string{
		"application/cloudevents+json",
		"application/cloudevents+json; charset=utf-8",
		"application/cloudevents+json;charset=utf-8",
		"application/cloudevents+json ; charset=utf-8",
		"application/cloudevents+json; charset=utf-8; boundary=x",
		// RFC 2045 §5.1 makes media types case-insensitive.
		"Application/CloudEvents+JSON",
		"APPLICATION/CLOUDEVENTS+JSON; CHARSET=UTF-8",
		// The ';' spelling event.ParseContentType explicitly accepts, and which the
		// publisher's codec lookup already resolves to the JSON codec. Routing it to
		// the binary unmarshaler dead-lettered every event from such a publisher.
		"application/cloudevents;json",
		"application/cloudevents; json",
		"application/cloudevents;json; charset=utf-8",
		"Application/CloudEvents;JSON",
	}
	for _, ct := range structured {
		if !isStructured(ct) {
			t.Errorf("isStructured(%q) = false, want true (a structured delivery sent to the binary path is dead-lettered)", ct)
		}
	}

	binary := []string{
		"",
		"application/protobuf",
		"application/json",
		"application/cloudevents+proto",
		"application/cloudevents-json",
		// Not a parameter boundary: a longer subtype is a different media type.
		"application/cloudevents+jsonx",
		// Structured mode but no subtype at all, and structured mode whose ';' slot
		// holds an ordinary media-type parameter instead of a subtype — neither names
		// the JSON envelope this marshaler writes.
		"application/cloudevents",
		"application/cloudevents; charset=utf-8",
	}
	for _, ct := range binary {
		if isStructured(ct) {
			t.Errorf("isStructured(%q) = true, want false", ct)
		}
	}
}
