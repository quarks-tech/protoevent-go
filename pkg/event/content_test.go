package event_test

import (
	"testing"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

// TestContentSubtype pins the parse table — most importantly that MALFORMED
// input never panics: the content type comes from the incoming message's
// metadata, and before the length guards a bare "application" or
// "application/cloudevents" crashed the subscriber instead of yielding
// ok=false, while "applicationfoo" was accepted as subtype "oo".
func TestContentSubtype(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"application/proto", "proto", true},
		{"application/json", "json", true},
		{"application/cloudevents+proto", "proto", true},
		// "application/cloudevents;json" is the structured-mode spelling, so the
		// first ';' field CAN be a subtype — but a parameter is not one. Returning
		// "charset=utf-8" as a codec name resolved to no codec while reporting
		// ok=true, so the caller saw an unprocessable event rather than "no subtype".
		{"application/cloudevents;json", "json", true},
		{"application/cloudevents; json", "json", true},
		{"application/cloudevents;charset=utf-8", "", false},
		{"application/cloudevents; charset=utf-8", "", false},
		{"application/cloudevents;json;charset=utf-8", "json", true},

		// RFC 2045 parameters belong to the media type, not the codec name:
		// leaving them attached yields a name no registered codec matches, so a
		// perfectly ordinary JSON payload is rejected as unprocessable.
		{"application/cloudevents+json; charset=utf-8", "json", true},
		{"application/cloudevents+json;charset=utf-8", "json", true},
		{"application/json; charset=utf-8", "json", true},
		{"application/proto;charset=utf-8", "proto", true},
		{"application/cloudevents+json ; charset=utf-8", "json", true},
		{"application/cloudevents+json; charset=utf-8; boundary=x", "json", true},

		// A parameter with no subtype in front of it still carries no codec name.
		{"application/; charset=utf-8", "", false},
		{"application/cloudevents+; charset=utf-8", "", false},

		// Media types are case-insensitive (RFC 2045 §5.1), and the codec registry
		// stores every codec under a lowercased name. A case variant that resolved
		// to no codec would have its deliveries rejected as unprocessable.
		{"application/JSON", "json", true},
		{"Application/Proto", "proto", true},
		{"APPLICATION/PROTO", "proto", true},
		{"Application/CloudEvents+JSON", "json", true},
		{"application/cloudevents+JSON; CharSet=UTF-8", "json", true},

		// Panic regressions: exact prefixes with nothing after them.
		{"application", "", false},
		{"application/", "", false},
		{"application/cloudevents", "", false},
		{"application/cloudevents+", "", false},

		// False-accept regression: prefix match without the "/" delimiter.
		{"applicationfoo", "", false},
		{"application/cloudeventsfoo", "", false},

		{"", "", false},
		{"text/plain", "", false},
	}
	for _, tc := range cases {
		got, ok := event.ContentSubtype(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("ContentSubtype(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}
