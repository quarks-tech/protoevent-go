package sequence_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/sequence"
)

// senderFunc adapts a func to eventbus.Sender.
type senderFunc func(context.Context, *event.Metadata, []byte) error

func (f senderFunc) Send(ctx context.Context, md *event.Metadata, d []byte) error {
	return f(ctx, md, d)
}

// mdSeq encodes/decodes a seq into metadata ID for send-order assertions. The
// fake store sets Metadata.ID to the decimal Seq at sequence time.
func mdSeq(md *event.Metadata) int64 {
	n, _ := strconv.ParseInt(md.ID, 10, 64)
	return n
}

func TestDefaultOptions(t *testing.T) {
	o := sequence.DefaultOptions()
	if o.BatchSize != 100 {
		t.Fatalf("BatchSize = %d, want 100", o.BatchSize)
	}
	if o.SequenceBatchSize != 1000 {
		t.Fatalf("SequenceBatchSize = %d, want 1000", o.SequenceBatchSize)
	}
	if o.PollInterval != time.Second {
		t.Fatalf("PollInterval = %v, want 1s", o.PollInterval)
	}
	if o.LeaseTTL != 15*time.Second {
		t.Fatalf("LeaseTTL = %v, want 15s", o.LeaseTTL)
	}
}

func TestOptionsApply(t *testing.T) {
	o := sequence.DefaultOptions()
	for _, opt := range []sequence.Option{
		sequence.WithBatchSize(50),
		sequence.WithSequenceBatchSize(500),
		sequence.WithoutSequencer(),
		sequence.WithRetention(7*24*time.Hour, 256, 5000),
	} {
		opt(&o)
	}
	if o.BatchSize != 50 || o.SequenceBatchSize != 500 {
		t.Fatalf("batch sizes not applied: %+v", o)
	}
	if !o.DisableSequencer {
		t.Fatal("WithoutSequencer did not set DisableSequencer")
	}
	if o.RetentionWindow != 7*24*time.Hour || o.RetentionSweepEvery != 256 || o.RetentionSweepBatch != 5000 {
		t.Fatalf("retention not applied: %+v", o)
	}
}

// --- in-memory contract fake ------------------------------------------------

// fakeStore models the SQL store's contract: a dense sequenced log, NULL-seq
// pending rows, monotone per-name offsets, and a single leader lock.
type fakeStore struct {
	mu      sync.Mutex
	pending []*outbox.Message // seq == 0, in (tx_start_ts,id) order as appended
	log     []*outbox.Message // seq assigned, ascending
	nextSeq int64
	offsets map[string]int64
	leader  string // holderID currently holding the lock ("" = free)
	seqErr  error
	listErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{nextSeq: 1, offsets: map[string]int64{}}
}

// append simulates a publish: an unsequenced row.
func (s *fakeStore) append(m *outbox.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, m)
}

func (s *fakeStore) SequenceMessages(_ context.Context, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seqErr != nil {
		return 0, s.seqErr
	}
	n := limit
	if n > len(s.pending) {
		n = len(s.pending)
	}
	for i := 0; i < n; i++ {
		m := s.pending[i]
		m.Seq = s.nextSeq
		m.Metadata.ID = strconv.FormatInt(s.nextSeq, 10)
		s.nextSeq++
		s.log = append(s.log, m)
	}
	s.pending = s.pending[n:]
	return n, nil
}

func (s *fakeStore) ListMessages(_ context.Context, afterSeq int64, limit int) ([]*outbox.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []*outbox.Message
	for _, m := range s.log {
		if m.Seq > afterSeq {
			out = append(out, m)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (s *fakeStore) Offset(_ context.Context, name string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.offsets[name], nil
}

func (s *fakeStore) CommitOffset(_ context.Context, name string, seq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq > s.offsets[name] { // GREATEST
		s.offsets[name] = seq
	}
	return nil
}

func (s *fakeStore) TryAcquireLeaderLock(_ context.Context, _, holderID string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leader == "" || s.leader == holderID {
		s.leader = holderID
		return true, nil
	}
	return false, nil
}

func (s *fakeStore) ReleaseLeaderLock(_ context.Context, _, holderID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leader == holderID {
		s.leader = ""
	}
	return nil
}

func msg(seq int64) *outbox.Message {
	return &outbox.Message{ID: "id", Seq: seq, Metadata: event.NewMetadata("t"), CreateTime: time.Now()}
}

// --- runtime tests ----------------------------------------------------------

func TestRunOnceSequencesThenDrainsSameTick(t *testing.T) {
	st := newFakeStore()
	st.append(msg(0))
	st.append(msg(0))
	st.append(msg(0))

	var got []int64
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		got = append(got, mdSeq(md))
		return nil
	})

	r := sequence.NewRelay("c", st, sender)
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("drained %d, want 3 (rows sequenced this tick must drain this tick)", len(got))
	}
	if off := st.offsets["c"]; off != 3 {
		t.Fatalf("offset = %d, want 3", off)
	}
}

func TestDrainLoopsUntilShortPage(t *testing.T) {
	st := newFakeStore()
	for i := 0; i < 250; i++ {
		st.append(msg(0))
	}
	var count int
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { count++; return nil })

	r := sequence.NewRelay("c", st, sender, sequence.WithBatchSize(100))
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if count != 250 {
		t.Fatalf("drained %d, want 250 (must loop past full pages)", count)
	}
}

func TestStopTheLaneOnSendError(t *testing.T) {
	st := newFakeStore()
	for i := 0; i < 5; i++ {
		st.append(msg(0))
	}
	var got []int64
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		if mdSeq(md) == 3 {
			return errors.New("boom")
		}
		got = append(got, mdSeq(md))
		return nil
	})

	r := sequence.NewRelay("c", st, sender)
	_ = r.RunOnce(context.Background())

	// Sent 1,2; stopped at 3. Offset must not advance past 2.
	if off := st.offsets["c"]; off != 2 {
		t.Fatalf("offset = %d, want 2 (stop-the-lane must not skip the failure)", off)
	}
	if len(got) != 2 {
		t.Fatalf("sent %v, want [1 2]", got)
	}
}

func TestNonLeaderDoesNothing(t *testing.T) {
	st := newFakeStore()
	st.leader = "someone-else"
	st.append(msg(0))
	sent := 0
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { sent++; return nil })

	r := sequence.NewRelay("c", st, sender, sequence.WithLeaderLockName("lock"))
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sent != 0 {
		t.Fatalf("non-leader sent %d, want 0", sent)
	}
}

func TestParkAndContinueAdvancesPastFailure(t *testing.T) {
	st := newFakeStore()
	for i := 0; i < 5; i++ {
		st.append(msg(0))
	}
	var got []int64
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		if mdSeq(md) == 3 {
			return errors.New("boom")
		}
		got = append(got, mdSeq(md))
		return nil
	})
	var parked []int64
	r := sequence.NewRelay("c", st, sender, sequence.WithErrorHandler(
		func(_ context.Context, m *outbox.Message, _ error) { parked = append(parked, m.Seq) },
	))
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// 3 parked; 1,2,4,5 delivered; offset advances to 5.
	if off := st.offsets["c"]; off != 5 {
		t.Fatalf("offset = %d, want 5 (park-and-continue advances past failure)", off)
	}
	if len(parked) != 1 || parked[0] != 3 {
		t.Fatalf("parked = %v, want [3]", parked)
	}
	if len(got) != 4 {
		t.Fatalf("delivered = %v, want 4 messages", got)
	}
}
