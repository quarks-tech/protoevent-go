package stream_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/stream"
)

// readmes are the two documents that show a stream.NewRelay call, relative to
// this package. The root one lives in a different MODULE, which is fine — this
// reads a file, it does not import anything — but it can legitimately be absent
// (a consumer who vendored only pkg/transport/outbox), so a missing file skips
// rather than fails.
var readmes = map[string]string{
	"README.md":                      "../../../../../README.md",
	"pkg/transport/outbox/README.md": "../../README.md",
}

// documentedOptions is the option set both READMEs pass to stream.NewRelay,
// written exactly as it appears there.
//
// Keep this in sync with the documents by hand — that is the point. The two
// tests below then guarantee the pair cannot drift apart silently: one asserts
// the READMEs still contain this text, the other asserts the options it
// describes actually construct a relay.
const documentedOptions = "stream.WithDrainWindow(time.Second),"

// TestREADMEExampleConstructs runs the constructor the READMEs tell people to
// copy.
//
// It exists because that snippet was WRONG and nothing noticed. It used to read
//
//	stream.WithDrainWindow(time.Second),
//	stream.WithLeaseTTL(15*time.Second),
//
// which fails validation outright — DrainWindow + OpTimeout (30s default) must
// be < LeaseTTL — so every reader who copied it got a hard startup error. The
// rule was already pinned by TestNewRelayRejectsOpTimeoutExceedingLease in this
// very package, using the same 15s/30s pair as its NEGATIVE case. A test proved
// the combination invalid while the documentation recommended it, because no
// test ever executed the documentation.
//
// The snippet became wrong without being edited: the default LeaseTTL was raised
// 15s -> 60s, and the example had been passing the old default explicitly, so it
// turned into a self-contradiction the moment the default moved. That is the
// failure mode this guards — documentation invalidated at a distance.
func TestREADMEExampleConstructs(t *testing.T) {
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })

	if _, err := stream.NewRelay("broker-publish", &fakeStore{}, sender,
		stream.WithDrainWindow(time.Second),
	); err != nil {
		t.Fatalf("the stream.NewRelay call both READMEs document does not construct: %v\n"+
			"Readers copy this verbatim, so it has to work against the CURRENT defaults — "+
			"fix the documents and %s here together.", err, documentedOptions)
	}
}

// TestREADMEExampleIsStillWhatIsDocumented closes the other direction: the test
// above only proves that ONE option set works, and is worthless if the documents
// have since moved to a different one.
func TestREADMEExampleIsStillWhatIsDocumented(t *testing.T) {
	for name, path := range readmes {
		b, err := os.ReadFile(path) //nolint:gosec // path is a constant from the map above, not input
		if os.IsNotExist(err) {
			// Root README is outside this module; absent in a partial checkout.
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		doc := string(b)

		if !strings.Contains(doc, "stream.NewRelay(") {
			t.Errorf("%s no longer shows a stream.NewRelay example; drop it from readmes "+
				"here, or restore the example", name)

			continue
		}
		if !strings.Contains(doc, documentedOptions) {
			t.Errorf("%s documents a stream.NewRelay option set this test does not cover.\n"+
				"Expected to find %q. Update documentedOptions and TestREADMEExampleConstructs "+
				"to match the new snippet, so the thing readers copy stays executed by a test.",
				name, documentedOptions)
		}
		// The specific regression that shipped: a lease shorter than the default
		// OpTimeout, which cannot validate at any DrainWindow.
		if strings.Contains(doc, "stream.WithLeaseTTL(15*time.Second)") &&
			!strings.Contains(doc, "stream.WithOpTimeout(") {
			t.Errorf("%s pairs stream.WithLeaseTTL(15s) with the default 30s OpTimeout, which "+
				"cannot validate (DrainWindow + OpTimeout must be < LeaseTTL). Either drop the "+
				"WithLeaseTTL line or add a WithOpTimeout small enough to fit.", name)
		}
	}
}
