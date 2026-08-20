package sequence_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/sequence"
)

// batchSenderFake is an eventbus.BatchSender whose per-batch answer a test dictates.
//
// It records the batches it was handed, which is how the "did the relay actually
// take the batch path" assertions are made without reaching into the relay.
type batchSenderFake struct {
	// answer decides one batch: how many of it were delivered, and the failure of
	// the message at that index. Defaults to delivering everything.
	answer func(msgs []eventbus.Outgoing) (int, error)

	batches [][]string // Metadata.ID per batch, in order
	single  int        // Send calls: must stay 0 whenever the batch path is taken
}

func (b *batchSenderFake) Send(context.Context, *event.Metadata, []byte) error {
	b.single++

	return nil
}

func (b *batchSenderFake) SendBatch(_ context.Context, msgs []eventbus.Outgoing) (int, error) {
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.Metadata.ID)
	}
	b.batches = append(b.batches, ids)

	if b.answer == nil {
		return len(msgs), nil
	}

	return b.answer(msgs)
}

// seededStore returns a store holding n pending messages, plus the Metadata.IDs
// they will carry once sequenced. The fake sequencer stamps Metadata.ID with the
// assigned seq (see fakeStore.SequenceMessages), so those ids double as an
// order-preserving label: asserting on them asserts on seq order.
func seededStore(n int) (*fakeStore, []string) {
	st := newFakeStore()
	ids := make([]string, 0, n)
	for i := range n {
		st.append(msg())
		ids = append(ids, strconv.Itoa(i+1))
	}

	return st, ids
}

// TestBatchSenderIsUsedWhenAvailable pins the capability handshake: a Sender that
// implements eventbus.BatchSender is driven through SendBatch, and one that does
// not keeps the per-message path. Discovery is a type assertion, so a method-set
// drift would silently fall back to the slow path with nothing to notice it.
func TestBatchSenderIsUsedWhenAvailable(t *testing.T) {
	st, ids := seededStore(5)
	snd := &batchSenderFake{}

	r, err := sequence.NewRelay("c", st, snd, sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(snd.batches) != 1 {
		t.Fatalf("SendBatch called %d times, want 1 — the whole page is one batch", len(snd.batches))
	}
	if got := snd.batches[0]; !equalIDs(got, ids) {
		t.Fatalf("batch = %v, want the page in seq order %v", got, ids)
	}
	if snd.single != 0 {
		t.Fatalf("Send called %d times: a BatchSender must not also be driven one message at a time", snd.single)
	}
	off, exists := st.offsetOf("c")
	if !exists {
		t.Fatal("no offset row was committed: the group would re-read the whole log next pass")
	}
	if off != 5 {
		t.Fatalf("committed offset = %d, want 5 (all delivered)", off)
	}
}

// TestBatchPartialFailureCommitsOnlyTheConfirmedPrefix is the safety property the
// whole capability rests on.
//
// SendBatch reports a CONTIGUOUS confirmed prefix. If the relay committed anything
// beyond it, the messages in the gap would be swept as delivered while no broker
// ever acknowledged them — a silent gap, the one failure mode with no alarm. Here
// the sender confirms 2 of 5 and fails on the third; the offset must land on seq 2
// and the next pass must re-read from there.
func TestBatchPartialFailureCommitsOnlyTheConfirmedPrefix(t *testing.T) {
	st, ids := seededStore(5)
	snd := &batchSenderFake{
		answer: func(msgs []eventbus.Outgoing) (int, error) {
			return 2, errors.New("broker nacked")
		},
	}

	r, err := sequence.NewRelay("c", st, snd, sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if off, _ := st.offsetOf("c"); off != 2 {
		t.Fatalf("committed offset = %d, want 2.\n"+
			"The sender confirmed only the first two messages; committing past them marks "+
			"events as delivered that the broker never acknowledged, and the retention sweep "+
			"then deletes them.", off)
	}

	// The next pass must retry from the unconfirmed message, not skip it.
	snd.answer = nil
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if len(snd.batches) != 2 {
		t.Fatalf("SendBatch called %d times, want 2", len(snd.batches))
	}
	if got, want := snd.batches[1], ids[2:]; !equalIDs(got, want) {
		t.Fatalf("retry batch = %v, want the unconfirmed suffix %v", got, want)
	}
	if off, _ := st.offsetOf("c"); off != 5 {
		t.Fatalf("committed offset after retry = %d, want 5", off)
	}
}

// TestBatchResumesAfterAParkedMessage pins that a parkable failure is a resumption
// point rather than the end of the page.
//
// The lane may advance past a message it durably parked, so the rest of the page is
// still deliverable in this pass. Falling back to one-message-at-a-time after a park
// (or worse, ending the page) would put a relay with a few unsendable rows back on
// the serial ceiling the batch path exists to escape.
func TestBatchResumesAfterAParkedMessage(t *testing.T) {
	st, ids := seededStore(5)

	// The third message is permanently unsendable; everything else is fine.
	snd := &batchSenderFake{
		answer: func(msgs []eventbus.Outgoing) (int, error) {
			for i, m := range msgs {
				if m.Metadata.ID == ids[2] {
					return i, fmt.Errorf("bad metadata: %w", event.ErrUnsendable)
				}
			}

			return len(msgs), nil
		},
	}

	var parked []string
	r, err := sequence.NewRelay("c", st, snd,
		sequence.WithStartFromBeginning(),
		sequence.WithPoisonHandler(func(_ context.Context, m *outbox.Message, _ error) error {
			parked = append(parked, m.Metadata.ID)

			return nil
		}),
	)
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(parked) != 1 || parked[0] != ids[2] {
		t.Fatalf("parked = %v, want exactly the unsendable message %q", parked, ids[2])
	}
	if len(snd.batches) != 2 {
		t.Fatalf("SendBatch called %d times, want 2 (the prefix, then the suffix after the park)",
			len(snd.batches))
	}
	if got, want := snd.batches[1], ids[3:]; !equalIDs(got, want) {
		t.Fatalf("post-park batch = %v, want the remaining suffix %v — a park must not end the page "+
			"or drop the relay back to one message at a time", got, want)
	}
	if off, _ := st.offsetOf("c"); off != 5 {
		t.Fatalf("committed offset = %d, want 5 (four sent, one confirmed-parked)", off)
	}
}

// TestBatchSenderOverReportingIsClamped defends the durable watermark against a
// buggy transport.
//
// sent is used to index positions and to move a committed offset, so a Sender that
// returns more than it was given would either panic the relay goroutine or advance
// the watermark past events nobody sent. Neither is an acceptable response to
// someone else's bug.
func TestBatchSenderOverReportingIsClamped(t *testing.T) {
	st, _ := seededStore(3)
	snd := &batchSenderFake{
		answer: func(msgs []eventbus.Outgoing) (int, error) { return len(msgs) + 7, nil },
	}

	r, err := sequence.NewRelay("c", st, snd, sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if off, _ := st.offsetOf("c"); off != 3 {
		t.Fatalf("committed offset = %d, want 3 — an over-reported count must be clamped to the "+
			"batch it was given, never allowed to move the watermark past it", off)
	}
}

// TestBatchSenderShortWithoutErrorStopsTheLane pins the other half of the contract.
//
// "Fewer delivered than asked for" and "no error" is a combination that cannot both
// be true, and the failure it produces is the quiet kind: the relay would commit the
// prefix, re-read the same suffix next tick, deliver nothing, and report no error —
// a stalled relay that looks idle. It must stop the lane with something an operator
// can read instead.
func TestBatchSenderShortWithoutErrorStopsTheLane(t *testing.T) {
	st, _ := seededStore(4)
	snd := &batchSenderFake{
		answer: func([]eventbus.Outgoing) (int, error) { return 1, nil },
	}

	var reported []error
	obs := relay.Observer{OnError: func(_ string, err error) { reported = append(reported, err) }}

	r, err := sequence.NewRelay("c", st, snd,
		sequence.WithStartFromBeginning(), sequence.WithObserver(obs))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if off, _ := st.offsetOf("c"); off != 1 {
		t.Fatalf("committed offset = %d, want 1 (only the reported prefix)", off)
	}
	if len(reported) == 0 {
		t.Fatal("a Sender that delivered fewer messages than it was given and returned no error " +
			"produced no OnError: the relay would stall silently, which is the one thing it must not do")
	}
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
