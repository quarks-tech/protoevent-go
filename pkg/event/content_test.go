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
		{"application/cloudevents;charset=utf-8", "charset=utf-8", true},

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
