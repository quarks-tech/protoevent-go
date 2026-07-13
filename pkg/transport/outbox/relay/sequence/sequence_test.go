package sequence_test

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
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

// noopSender is a shared no-op sender for tests that exercise paths (offset,
// sequencer, sweep errors, etc.) which never reach Sender.Send.
var noopSender = senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })

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
	if !o.SequencerDisabled {
		t.Fatal("WithoutSequencer did not set SequencerDisabled")
	}
	if o.RetentionWindow != 7*24*time.Hour || o.RetentionSweepEvery != 256 || o.RetentionSweepBatch != 5000 {
		t.Fatalf("retention not applied: %+v", o)
	}
}

func TestNewRelayRejectsZeroPollInterval(t *testing.T) {
	// A zero PollInterval previously panicked inside time.NewTicker; it must
	// now be rejected at construction time.
	_, err := sequence.NewRelay("c", newFakeStore(), nil, sequence.WithPollInterval(0))
	if err == nil {
		t.Fatal("expected error for zero PollInterval, got nil")
	}
}

func TestNewRelayRejectsZeroBatchSize(t *testing.T) {
	_, err := sequence.NewRelay("c", newFakeStore(), nil, sequence.WithBatchSize(0))
	if err == nil {
		t.Fatal("expected error for zero BatchSize, got nil")
	}
}

func TestNewRelayRejectsZeroSequenceBatchSize(t *testing.T) {
	_, err := sequence.NewRelay("c", newFakeStore(), nil, sequence.WithSequenceBatchSize(0))
	if err == nil {
		t.Fatal("expected error for zero SequenceBatchSize, got nil")
	}
}

func TestNewRelayRejectsZeroLeaseTTL(t *testing.T) {
	_, err := sequence.NewRelay("c", newFakeStore(), nil, sequence.WithLeaseTTL(0))
	if err == nil {
		t.Fatal("expected error for zero LeaseTTL, got nil")
	}
}

func TestNewRelayRejectsRetentionWindowWithoutSweepCadence(t *testing.T) {
	// A positive RetentionWindow with a zero sweep cadence/batch would make
	// retention silently non-functional (maybeSweep's `<= 0` guard always
	// skips it), so it must be rejected at construction time.
	_, err := sequence.NewRelay("c", newFakeStore(), nil, sequence.WithRetention(24*time.Hour, 0, 0))
	if err == nil {
		t.Fatal("expected error for RetentionWindow with zero sweep cadence, got nil")
	}
}

func TestNewRelayAcceptsDefaults(t *testing.T) {
	r, err := sequence.NewRelay("c", newFakeStore(), noopSender)
	if err != nil || r == nil {
		t.Fatalf("NewRelay with defaults: r=%v err=%v", r, err)
	}
}

func TestNewRelayRejectsNilStore(t *testing.T) {
	_, err := sequence.NewRelay("c", nil, noopSender)
	if err == nil {
		t.Fatal("expected error for nil store, got nil")
	}
}

func TestNewRelayRejectsNilSender(t *testing.T) {
	_, err := sequence.NewRelay("c", newFakeStore(), nil)
	if err == nil {
		t.Fatal("expected error for nil sender, got nil")
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

	offsetErr     error
	initOffsetErr error
	commitErr     error

	// poisonSeq, when non-zero, marks that row as undecodable: ListMessages
	// returns the decoded prefix before it plus a *sequence.DecodeError, per
	// the Store contract.
	poisonSeq int64

	seqCalls int // number of SequenceMessages invocations, for loop-count assertions

	sweepErr        error
	sweepCalls      int
	lastSweepBefore time.Time
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
	s.seqCalls++
	if s.seqErr != nil {
		return 0, s.seqErr
	}
	n := min(limit, len(s.pending))
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
	// The log is Seq-ascending, so binary-search the page start instead of
	// scanning from index 0: with a linear scan the benchmarks would measure
	// the fake's O(N^2/B) walk, not the relay.
	start := sort.Search(len(s.log), func(i int) bool { return s.log[i].Seq > afterSeq })
	var out []*outbox.Message
	for _, m := range s.log[start:] {
		if s.poisonSeq != 0 && m.Seq == s.poisonSeq {
			return out, &sequence.DecodeError{ID: m.ID, Seq: m.Seq, Err: errors.New("bad metadata")}
		}
		out = append(out, m)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *fakeStore) Offset(_ context.Context, name string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.offsetErr != nil {
		return 0, s.offsetErr
	}
	return s.offsets[name], nil
}

func (s *fakeStore) CommitOffset(_ context.Context, name string, seq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.commitErr != nil {
		return s.commitErr
	}
	if seq > s.offsets[name] { // GREATEST
		s.offsets[name] = seq
	}
	return nil
}

// InitOffsetLatest mirrors the SQL store's insert-if-absent: create name's
// offset row at the current max sequenced seq ONLY if no row exists, and
// return the effective committed offset. An existing row — even at 0 — is a
// committed position and is never modified.
func (s *fakeStore) InitOffsetLatest(_ context.Context, name string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initOffsetErr != nil {
		return 0, s.initOffsetErr
	}
	if off, ok := s.offsets[name]; ok {
		return off, nil
	}
	maxSeq := int64(0)
	if n := len(s.log); n > 0 {
		maxSeq = s.log[n-1].Seq
	}
	s.offsets[name] = maxSeq
	return maxSeq, nil
}

// SweepMessages implements sequence.RetentionStore: it records invocation
// count and the `before` cutoff for retention-cadence assertions.
func (s *fakeStore) SweepMessages(_ context.Context, before time.Time, _ int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepCalls++
	s.lastSweepBefore = before
	if s.sweepErr != nil {
		return 0, s.sweepErr
	}
	return 0, nil
}

// snapshotSweep reads the sweep counters under mu.
func (s *fakeStore) snapshotSweep() (calls int, before time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sweepCalls, s.lastSweepBefore
}

// snapshotSeqCalls reads the SequenceMessages invocation count under mu.
func (s *fakeStore) snapshotSeqCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seqCalls
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

// leaderHolder returns the current lock holder ("" if free), guarded by mu.
func (s *fakeStore) leaderHolder() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leader
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

	r, err := sequence.NewRelay("c", st, sender, sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
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
	for range 250 {
		st.append(msg(0))
	}
	var count int
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { count++; return nil })

	r, err := sequence.NewRelay("c", st, sender, sequence.WithBatchSize(100), sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if count != 250 {
		t.Fatalf("drained %d, want 250 (must loop past full pages)", count)
	}
}

func TestStopTheLaneOnSendError(t *testing.T) {
	st := newFakeStore()
	for range 5 {
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

	r, err := sequence.NewRelay("c", st, sender, sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	_ = r.RunOnce(t.Context())

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

	r, err := sequence.NewRelay("c", st, sender, sequence.WithLeaderLockName("lock"))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sent != 0 {
		t.Fatalf("non-leader sent %d, want 0", sent)
	}
}

func TestParkAndContinueAdvancesPastFailure(t *testing.T) {
	st := newFakeStore()
	for range 5 {
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
	r, err := sequence.NewRelay("c", st, sender, sequence.WithStartFromBeginning(), sequence.WithErrorHandler(
		func(_ context.Context, m *outbox.Message, _ error) { parked = append(parked, m.Seq) },
	))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
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

// recordingObserver tracks whether ObserveError was ever called.
type recordingObserver struct {
	mu          sync.Mutex
	errObserved bool
}

func (o *recordingObserver) ObserveDrained(string, int, time.Duration, bool) {}
func (o *recordingObserver) ObserveSequenced(string, int)                    {}
func (o *recordingObserver) ObserveError(string, error) {
	o.mu.Lock()
	o.errObserved = true
	o.mu.Unlock()
}

// ctxCancelingLeaderStore cancels the ctx passed to Run (via the embedded
// cancel func) from inside TryAcquireLeaderLock, then returns ctx.Err() once
// it observes the cancellation. This reproduces a leader-lock call that fails
// with context.Canceled mid-RunOnce (as opposed to Run's own ctx.Done() select
// case, which would exit the loop before ever calling RunOnce again).
type ctxCancelingLeaderStore struct {
	*fakeStore
	cancel context.CancelFunc
}

func (s ctxCancelingLeaderStore) TryAcquireLeaderLock(ctx context.Context, _, _ string, _ time.Duration) (bool, error) {
	s.cancel()
	<-ctx.Done()
	return false, ctx.Err()
}

// TestRunDoesNotReportErrorOnShutdown verifies that a context-cancellation
// error surfacing mid-RunOnce (a planned shutdown) is not reported to the
// Observer/Logger as a pass-level error.
func TestRunDoesNotReportErrorOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	st := ctxCancelingLeaderStore{fakeStore: newFakeStore(), cancel: cancel}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	obs := &recordingObserver{}
	r, err := sequence.NewRelay("c", st, sender, sequence.WithObserver(obs), sequence.WithPollInterval(time.Millisecond))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	runErr := r.Run(ctx)
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", runErr)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if obs.errObserved {
		t.Fatal("ObserveError called on planned shutdown, want none")
	}
}

// signalingObserver notifies a channel (non-blocking) on every ObserveError,
// so a test can wait for the first occurrence without polling.
type signalingObserver struct {
	ch chan struct{}
}

func (o *signalingObserver) ObserveDrained(string, int, time.Duration, bool) {}
func (o *signalingObserver) ObserveSequenced(string, int)                    {}
func (o *signalingObserver) ObserveError(string, error) {
	select {
	case o.ch <- struct{}{}:
	default:
	}
}

// TestRunObservesOpLevelDeadlineExceededWhileCtxAlive proves the shutdown-quiet
// path is gated on run-context liveness, not error identity: a genuine
// op-level context.DeadlineExceeded returned by the store while ctx is still
// alive must be observed as a real, recurring error — not silently swallowed
// as a planned shutdown.
func TestRunObservesOpLevelDeadlineExceededWhileCtxAlive(t *testing.T) {
	st := newFakeStore()
	st.seqErr = context.DeadlineExceeded
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	obs := &signalingObserver{ch: make(chan struct{}, 1)}

	r, err := sequence.NewRelay("c", st, sender, sequence.WithObserver(obs), sequence.WithPollInterval(5*time.Millisecond))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	select {
	case <-obs.ch:
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("ObserveError was not called for an op-level DeadlineExceeded while ctx was alive")
	}

	cancel()
	if runErr := <-done; !errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", runErr)
	}
}

// TestNewGroupStartsAtLatestByDefault proves parity with the stream runtime's
// start-at-now: a brand-new consumer group (no committed offset) must not
// replay the retained log. Only events sequenced AFTER the group's first
// RunOnce are delivered.
func TestNewGroupStartsAtLatestByDefault(t *testing.T) {
	st := newFakeStore()
	for range 3 {
		st.append(msg(0))
	}
	if _, err := st.SequenceMessages(context.Background(), 100); err != nil {
		t.Fatalf("seed sequence: %v", err)
	}

	var got []int64
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		got = append(got, mdSeq(md))
		return nil
	})

	r, err := sequence.NewRelay("fresh", st, sender) // no WithStartFromBeginning
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("delivered %v on a fresh group, want nothing (latest-default must skip the retained log)", got)
	}
	if off := st.offsets["fresh"]; off != 3 {
		t.Fatalf("offset = %d, want 3 (initialized to current max seq)", off)
	}

	st.append(msg(0))
	st.append(msg(0))
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(got) != 2 || got[0] != 4 || got[1] != 5 {
		t.Fatalf("delivered = %v, want [4 5] (only events sequenced after the group started)", got)
	}
}

// TestRunTicksThenReleasesOnCancel exercises Run's fake-clock lifecycle end to
// end: the ticker drives repeated drains of a pending message, and canceling
// releases the leader lock so a planned shutdown fails over quickly. Uses
// testing/synctest's fake clock so the 1s PollInterval elapses instantly and
// deterministically.
func TestRunTicksThenReleasesOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := newFakeStore()
		st.append(msg(0))

		var delivered atomic.Int64
		sender := senderFunc(func(context.Context, *event.Metadata, []byte) error {
			delivered.Add(1)
			return nil
		})

		r, err := sequence.NewRelay("c", st, sender,
			sequence.WithPollInterval(time.Second), sequence.WithStartFromBeginning())
		if err != nil {
			t.Fatalf("NewRelay: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		go func() { _ = r.Run(ctx) }()

		time.Sleep(1500 * time.Millisecond)
		synctest.Wait()

		if delivered.Load() < 1 {
			t.Fatalf("delivered = %d, want >= 1 (ticker must have driven at least one drain)", delivered.Load())
		}

		cancel()
		synctest.Wait()

		if holder := st.leaderHolder(); holder != "" {
			t.Fatalf("leader lock holder = %q, want \"\" (Run must release on cancel)", holder)
		}
	})
}

// --- recordingHandler ---------------------------------------------------------

// recordingHandler is a slog.Handler recording every record's message, so
// tests can assert the relay's *slog.Logger was (or wasn't) invoked.
type recordingHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, rec slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, rec.Message)
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.msgs))
	copy(out, h.msgs)
	return out
}

// --- noRetentionStore ---------------------------------------------------------

// noRetentionStore wraps a *fakeStore but deliberately does NOT expose
// SweepMessages (no embedding, so no method promotion), proving maybeSweep
// treats a store lacking sequence.RetentionStore as retention-disabled.
type noRetentionStore struct{ inner *fakeStore }

func (s noRetentionStore) ListMessages(ctx context.Context, afterSeq int64, limit int) ([]*outbox.Message, error) {
	return s.inner.ListMessages(ctx, afterSeq, limit)
}

func (s noRetentionStore) Offset(ctx context.Context, name string) (int64, error) {
	return s.inner.Offset(ctx, name)
}

func (s noRetentionStore) CommitOffset(ctx context.Context, name string, seq int64) error {
	return s.inner.CommitOffset(ctx, name, seq)
}

func (s noRetentionStore) InitOffsetLatest(ctx context.Context, name string) (int64, error) {
	return s.inner.InitOffsetLatest(ctx, name)
}

func (s noRetentionStore) SequenceMessages(ctx context.Context, limit int) (int, error) {
	return s.inner.SequenceMessages(ctx, limit)
}

func (s noRetentionStore) TryAcquireLeaderLock(ctx context.Context, name, holderID string, ttl time.Duration) (bool, error) {
	return s.inner.TryAcquireLeaderLock(ctx, name, holderID, ttl)
}

func (s noRetentionStore) ReleaseLeaderLock(ctx context.Context, name, holderID string) error {
	return s.inner.ReleaseLeaderLock(ctx, name, holderID)
}

// --- maybeSweep ---------------------------------------------------------------

// TestMaybeSweepRunsOnCadence proves the retention sweep fires exactly every
// RetentionSweepEvery-th RunOnce, with a `before` cutoff approximately
// now-minus-window.
func TestMaybeSweepRunsOnCadence(t *testing.T) {
	st := newFakeStore()
	window := 24 * time.Hour
	r, err := sequence.NewRelay("c", st, noopSender, sequence.WithRetention(window, 3, 100))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	for i := 1; i <= 6; i++ {
		if err := r.RunOnce(t.Context()); err != nil {
			t.Fatalf("RunOnce[%d]: %v", i, err)
		}
		calls, before := st.snapshotSweep()
		wantCalls := i / 3
		if calls != wantCalls {
			t.Fatalf("after RunOnce %d: sweepCalls = %d, want %d (every 3rd tick)", i, calls, wantCalls)
		}
		if wantCalls > 0 {
			wantBefore := time.Now().Add(-window)
			if diff := wantBefore.Sub(before); diff < -time.Minute || diff > time.Minute {
				t.Fatalf("sweep `before` = %v, want ~%v (within a minute)", before, wantBefore)
			}
		}
	}
}

// TestMaybeSweepNoopWithoutRetentionStore proves a store lacking
// sequence.RetentionStore never gets a SweepMessages call and RunOnce does
// not panic, even with RetentionWindow configured via a capable neighbor.
func TestMaybeSweepNoopWithoutRetentionStore(t *testing.T) {
	inner := newFakeStore()
	st := noRetentionStore{inner: inner}
	r, err := sequence.NewRelay("c", st, noopSender, sequence.WithRetention(time.Hour, 1, 100))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	for i := range 3 {
		if err := r.RunOnce(t.Context()); err != nil {
			t.Fatalf("RunOnce[%d]: %v", i, err)
		}
	}
	if calls, _ := inner.snapshotSweep(); calls != 0 {
		t.Fatalf("sweepCalls = %d, want 0 (store lacks RetentionStore)", calls)
	}
}

// TestMaybeSweepErrorPropagates proves RunOnce surfaces a sweep error.
func TestMaybeSweepErrorPropagates(t *testing.T) {
	sentinel := errors.New("sweep boom")
	st := newFakeStore()
	st.sweepErr = sentinel
	r, err := sequence.NewRelay("c", st, noopSender, sequence.WithRetention(time.Hour, 1, 100))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	runErr := r.RunOnce(t.Context())
	if !errors.Is(runErr, sentinel) {
		t.Fatalf("RunOnce err = %v, want %v", runErr, sentinel)
	}
}

// --- sequence() looping and error propagation ---------------------------------

// TestSequenceLoopsWhileFull proves sequence() keeps calling SequenceMessages
// while a pass returns a full page, so a burst larger than SequenceBatchSize
// is fully sequenced within one RunOnce/tick.
func TestSequenceLoopsWhileFull(t *testing.T) {
	st := newFakeStore()
	for range 25 {
		st.append(msg(0))
	}
	r, err := sequence.NewRelay("c", st, noopSender,
		sequence.WithSequenceBatchSize(10), sequence.WithBatchSize(1000))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if calls := st.snapshotSeqCalls(); calls <= 1 {
		t.Fatalf("SequenceMessages called %d times, want > 1 (25 pending / batch 10 must loop)", calls)
	}
	if len(st.log) != 25 {
		t.Fatalf("sequenced %d, want 25 (all pending drained across loop iterations)", len(st.log))
	}
}

// TestSequenceStopsOnCtxCancellationMidLoop proves the loop-tail ctx.Err()
// check aborts a still-full-batch sequencer loop instead of spinning forever.
// The fake store ignores ctx in SequenceMessages, so an already-canceled ctx
// is only observed via the explicit ctx.Err() check between iterations.
func TestSequenceStopsOnCtxCancellationMidLoop(t *testing.T) {
	st := newFakeStore()
	for range 4 {
		st.append(msg(0))
	}
	r, err := sequence.NewRelay("c", st, noopSender, sequence.WithSequenceBatchSize(2))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	runErr := r.RunOnce(ctx)
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("RunOnce err = %v, want context.Canceled", runErr)
	}
	if calls := st.snapshotSeqCalls(); calls != 1 {
		t.Fatalf("SequenceMessages called %d times, want 1 (must stop after the first full page once ctx is canceled)", calls)
	}
}

// TestSequenceSequencerErrorPropagates proves RunOnce surfaces a
// SequenceMessages error directly.
func TestSequenceSequencerErrorPropagates(t *testing.T) {
	sentinel := errors.New("sequencer boom")
	st := newFakeStore()
	st.seqErr = sentinel
	r, err := sequence.NewRelay("c", st, noopSender)
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	runErr := r.RunOnce(t.Context())
	if !errors.Is(runErr, sentinel) {
		t.Fatalf("RunOnce err = %v, want %v", runErr, sentinel)
	}
}

// TestWithoutSequencerSkipsSequencing proves WithoutSequencer makes
// sequence() a true no-op (r.sequencer == nil) even though the underlying
// store implements SequencerStore.
func TestWithoutSequencerSkipsSequencing(t *testing.T) {
	st := newFakeStore()
	st.append(msg(0))
	r, err := sequence.NewRelay("c", st, noopSender, sequence.WithoutSequencer(), sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if calls := st.snapshotSeqCalls(); calls != 0 {
		t.Fatalf("SequenceMessages called %d times, want 0 (WithoutSequencer)", calls)
	}
}

// --- drain() error propagation -------------------------------------------------

// TestRunOnceOffsetErrorPropagates proves a Store.Offset error surfaces
// through drain() and RunOnce.
func TestRunOnceOffsetErrorPropagates(t *testing.T) {
	sentinel := errors.New("offset boom")
	st := newFakeStore()
	st.offsetErr = sentinel
	r, err := sequence.NewRelay("c", st, noopSender)
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	runErr := r.RunOnce(t.Context())
	if !errors.Is(runErr, sentinel) {
		t.Fatalf("RunOnce err = %v, want %v", runErr, sentinel)
	}
}

// TestRunOnceInitOffsetLatestErrorPropagates proves an InitOffsetLatest error
// (hit for a brand-new consumer group at offset 0) surfaces through drain()
// and RunOnce.
func TestRunOnceInitOffsetLatestErrorPropagates(t *testing.T) {
	sentinel := errors.New("init offset boom")
	st := newFakeStore()
	st.initOffsetErr = sentinel
	r, err := sequence.NewRelay("c", st, noopSender)
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	runErr := r.RunOnce(t.Context())
	if !errors.Is(runErr, sentinel) {
		t.Fatalf("RunOnce err = %v, want %v", runErr, sentinel)
	}
}

// TestRunOnceListMessagesErrorPropagates proves a Store.ListMessages error
// surfaces through drain() and, in turn, through RunOnce's own
// `if err := r.drain(ctx); err != nil` branch.
func TestRunOnceListMessagesErrorPropagates(t *testing.T) {
	sentinel := errors.New("list boom")
	st := newFakeStore()
	st.listErr = sentinel
	r, err := sequence.NewRelay("c", st, noopSender)
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	runErr := r.RunOnce(t.Context())
	if !errors.Is(runErr, sentinel) {
		t.Fatalf("RunOnce err = %v, want %v", runErr, sentinel)
	}
}

// TestRunOnceCommitOffsetErrorPropagates proves a Store.CommitOffset error
// surfaces through drain() and RunOnce after a successful send.
func TestRunOnceCommitOffsetErrorPropagates(t *testing.T) {
	sentinel := errors.New("commit boom")
	st := newFakeStore()
	st.append(msg(0))
	st.commitErr = sentinel
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	r, err := sequence.NewRelay("c", st, sender, sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	runErr := r.RunOnce(t.Context())
	if !errors.Is(runErr, sentinel) {
		t.Fatalf("RunOnce err = %v, want %v", runErr, sentinel)
	}
}

// TestDrainStopsImmediatelyOnPreCanceledCtx proves drain's per-message ctx
// check stops the lane before any send when ctx is already canceled: an
// already-dead run context must not walk even the first page.
func TestDrainStopsImmediatelyOnPreCanceledCtx(t *testing.T) {
	st := newFakeStore()
	for range 4 {
		st.append(msg(0))
	}
	if _, err := st.SequenceMessages(context.Background(), 100); err != nil {
		t.Fatalf("seed sequence: %v", err)
	}
	var sent int
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { sent++; return nil })
	r, err := sequence.NewRelay("c", st, sender,
		sequence.WithBatchSize(2), sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	runErr := r.RunOnce(ctx)
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("RunOnce err = %v, want context.Canceled", runErr)
	}
	if sent != 0 {
		t.Fatalf("sent %d messages on a pre-canceled ctx, want 0", sent)
	}
	if off := st.offsets["c"]; off != 0 {
		t.Fatalf("offset = %d, want 0 (nothing sent, nothing committed)", off)
	}
}

// --- WithLogger ----------------------------------------------------------------

func TestWithLoggerNilIsNoop(t *testing.T) {
	o := sequence.DefaultOptions()
	real := slog.New(&recordingHandler{})
	sequence.WithLogger(real)(&o)
	sequence.WithLogger(nil)(&o)
	if o.Logger != real {
		t.Fatal("WithLogger(nil) replaced a previously set logger")
	}
}

func TestWithLoggerReceivesTransientError(t *testing.T) {
	st := newFakeStore()
	for range 5 {
		st.append(msg(0))
	}
	h := &recordingHandler{}
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		if mdSeq(md) == 3 {
			return errors.New("boom")
		}
		return nil
	})
	r, err := sequence.NewRelay("c", st, sender, sequence.WithLogger(slog.New(h)), sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	_ = r.RunOnce(t.Context())
	if msgs := h.snapshot(); len(msgs) == 0 {
		t.Fatal("custom logger was never called on send failure")
	}
}

// --- new-validation, lease-renewal, shutdown, and poison-row coverage ----------

func TestNewRelayRejectsEmptyName(t *testing.T) {
	// name is the offset-row key and the default leader-lock name; two
	// empty-named relays would silently share both.
	_, err := sequence.NewRelay("", newFakeStore(), noopSender)
	if err == nil {
		t.Fatal("expected error for empty name, got nil")
	}
}

func TestNewRelayRejectsPollIntervalNotBelowHalfLeaseTTL(t *testing.T) {
	// The lease is renewed once per tick, so PollInterval must be < LeaseTTL/2
	// or the lease expires between renewals and leadership flaps every tick.
	_, err := sequence.NewRelay("c", newFakeStore(), noopSender,
		sequence.WithPollInterval(time.Second), sequence.WithLeaseTTL(2*time.Second))
	if err == nil {
		t.Fatal("expected error for PollInterval >= LeaseTTL/2, got nil")
	}
}

// TestNewRelayRedefaultsNilObserverAndLogger proves a raw Option that nils
// Observer/Logger (bypassing the With* nil guards) does not leave the runtime
// with a latent nil-deref: NewRelay re-defaults both after applying options.
func TestNewRelayRedefaultsNilObserverAndLogger(t *testing.T) {
	st := newFakeStore()
	st.append(msg(0))
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error {
		return errors.New("boom") // exercises the Observer+Logger error path
	})
	r, err := sequence.NewRelay("c", st, sender, sequence.WithStartFromBeginning(),
		func(o *sequence.Options) { o.Observer = nil; o.Logger = nil })
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil { // must not panic
		t.Fatalf("RunOnce: %v", err)
	}
}

// revokingLeaderStore wraps fakeStore's leader lock but only lets the first
// `allowed` TryAcquireLeaderLock calls succeed; later renewals report the lock
// as lost, simulating leadership revoked mid-pass.
type revokingLeaderStore struct {
	*fakeStore
	allowed atomic.Int32 // remaining acquires that may succeed
}

func (s *revokingLeaderStore) TryAcquireLeaderLock(ctx context.Context, name, holderID string, ttl time.Duration) (bool, error) {
	if s.allowed.Add(-1) < 0 {
		return false, nil
	}
	return s.fakeStore.TryAcquireLeaderLock(ctx, name, holderID, ttl)
}

// TestDrainStopsWhenLeadershipLostBetweenPages proves the between-pages lease
// renewal: when the renewal after the first full page reports leadership lost,
// the relay ends the pass cleanly (nil error) with only that page committed,
// bounding stale-leader overlap to a single page.
func TestDrainStopsWhenLeadershipLostBetweenPages(t *testing.T) {
	st := &revokingLeaderStore{fakeStore: newFakeStore()}
	st.allowed.Store(1) // RunOnce's opening acquire succeeds; the renewal fails
	for range 6 {
		st.append(msg(0))
	}
	var sent int
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { sent++; return nil })

	r, err := sequence.NewRelay("c", st, sender,
		sequence.WithBatchSize(2), sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if runErr := r.RunOnce(t.Context()); runErr != nil {
		t.Fatalf("RunOnce = %v, want nil (losing leadership is not an error)", runErr)
	}
	if sent != 2 {
		t.Fatalf("sent = %d, want 2 (must stop after the first page once leadership is lost)", sent)
	}
	if off := st.offsets["c"]; off != 2 {
		t.Fatalf("offset = %d, want 2 (first page committed before the pass ended)", off)
	}
}

// TestCancelDuringSendDoesNotPark proves drain's send-failure branch treats a
// failure with a canceled run ctx as shutdown, not a message fault: no
// ErrorHandler invocation, no offset advance past the last success.
func TestCancelDuringSendDoesNotPark(t *testing.T) {
	st := newFakeStore()
	for range 5 {
		st.append(msg(0))
	}
	ctx, cancel := context.WithCancel(t.Context())
	var sent []int64
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		if mdSeq(md) == 3 {
			cancel()
			return ctx.Err() // send aborted by the shutdown
		}
		sent = append(sent, mdSeq(md))
		return nil
	})
	var parked []int64
	r, err := sequence.NewRelay("c", st, sender, sequence.WithStartFromBeginning(),
		sequence.WithErrorHandler(func(_ context.Context, m *outbox.Message, _ error) {
			parked = append(parked, m.Seq)
		}))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	runErr := r.RunOnce(ctx)
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("RunOnce err = %v, want context.Canceled", runErr)
	}
	if len(parked) != 0 {
		t.Fatalf("parked = %v, want none (shutdown must not park healthy messages)", parked)
	}
	if off := st.offsets["c"]; off != 2 {
		t.Fatalf("offset = %d, want 2 (not advanced past the last success)", off)
	}
	if len(sent) != 2 {
		t.Fatalf("sent = %v, want [1 2]", sent)
	}
}

// TestCancelBetweenSendsStopsLane proves drain's per-message ctx check: a ctx
// canceled after a successful send stops the lane before the next message is
// even attempted (no fail-fast walk of the rest of the page).
func TestCancelBetweenSendsStopsLane(t *testing.T) {
	st := newFakeStore()
	for range 5 {
		st.append(msg(0))
	}
	ctx, cancel := context.WithCancel(t.Context())
	var sent []int64
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		sent = append(sent, mdSeq(md))
		if mdSeq(md) == 2 {
			cancel() // shutdown lands after this send succeeds
		}
		return nil
	})
	var parked []int64
	r, err := sequence.NewRelay("c", st, sender, sequence.WithStartFromBeginning(),
		sequence.WithErrorHandler(func(_ context.Context, m *outbox.Message, _ error) {
			parked = append(parked, m.Seq)
		}))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	runErr := r.RunOnce(ctx)
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("RunOnce err = %v, want context.Canceled", runErr)
	}
	if len(sent) != 2 {
		t.Fatalf("sent = %v, want [1 2] (no message attempted after cancellation)", sent)
	}
	if len(parked) != 0 {
		t.Fatalf("parked = %v, want none", parked)
	}
	if off := st.offsets["c"]; off != 2 {
		t.Fatalf("offset = %d, want 2 (committed through the last success)", off)
	}
}

// TestPoisonRowParkedWithErrorHandler proves DecodeError routing with an
// ErrorHandler: the decoded prefix is delivered, the poison row is parked
// exactly once, the offset advances past it, and rows after it are delivered.
func TestPoisonRowParkedWithErrorHandler(t *testing.T) {
	st := newFakeStore()
	for range 5 {
		st.append(msg(0))
	}
	st.poisonSeq = 3

	var got []int64
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		got = append(got, mdSeq(md))
		return nil
	})
	var parked []*outbox.Message
	var parkedErrs []error
	r, err := sequence.NewRelay("c", st, sender, sequence.WithStartFromBeginning(),
		sequence.WithErrorHandler(func(_ context.Context, m *outbox.Message, err error) {
			parked = append(parked, m)
			parkedErrs = append(parkedErrs, err)
		}))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(got) != 4 || got[0] != 1 || got[1] != 2 || got[2] != 4 || got[3] != 5 {
		t.Fatalf("delivered = %v, want [1 2 4 5]", got)
	}
	if len(parked) != 1 || parked[0].Seq != 3 {
		t.Fatalf("parked = %+v, want exactly the seq-3 poison row once", parked)
	}
	var de *sequence.DecodeError
	if !errors.As(parkedErrs[0], &de) || de.Seq != 3 {
		t.Fatalf("parked err = %v, want *sequence.DecodeError with Seq 3", parkedErrs[0])
	}
	if off := st.offsets["c"]; off != 5 {
		t.Fatalf("offset = %d, want 5 (advanced past the parked poison row)", off)
	}
}

// TestPoisonRowStopsLaneWithoutErrorHandler proves the default stop-the-lane
// behavior for a decode failure: the decoded prefix is delivered and
// committed, then the pass surfaces the *DecodeError and stops at the row.
func TestPoisonRowStopsLaneWithoutErrorHandler(t *testing.T) {
	st := newFakeStore()
	for range 5 {
		st.append(msg(0))
	}
	st.poisonSeq = 3

	var got []int64
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		got = append(got, mdSeq(md))
		return nil
	})
	r, err := sequence.NewRelay("c", st, sender, sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	runErr := r.RunOnce(t.Context())
	var de *sequence.DecodeError
	if !errors.As(runErr, &de) || de.Seq != 3 {
		t.Fatalf("RunOnce err = %v, want *sequence.DecodeError with Seq 3", runErr)
	}
	if len(got) != 2 {
		t.Fatalf("delivered = %v, want [1 2] (prefix only)", got)
	}
	if off := st.offsets["c"]; off != 2 {
		t.Fatalf("offset = %d, want 2 (lane stops at the poison row)", off)
	}
}

// drainCountObserver records every ObserveDrained count.
type drainCountObserver struct {
	mu     sync.Mutex
	counts []int
}

func (o *drainCountObserver) ObserveDrained(_ string, count int, _ time.Duration, _ bool) {
	o.mu.Lock()
	o.counts = append(o.counts, count)
	o.mu.Unlock()
}
func (o *drainCountObserver) ObserveSequenced(string, int) {}
func (o *drainCountObserver) ObserveError(string, error)   {}

// TestObserveDrainedExcludesParkedMessages pins the relay.Observer contract:
// ObserveDrained's count is successful sends only — a parked message is
// surfaced via ObserveError and not counted.
func TestObserveDrainedExcludesParkedMessages(t *testing.T) {
	st := newFakeStore()
	for range 5 {
		st.append(msg(0))
	}
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		if mdSeq(md) == 3 {
			return errors.New("boom")
		}
		return nil
	})
	obs := &drainCountObserver{}
	r, err := sequence.NewRelay("c", st, sender, sequence.WithStartFromBeginning(),
		sequence.WithObserver(obs),
		sequence.WithErrorHandler(func(context.Context, *outbox.Message, error) {}))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	obs.mu.Lock()
	defer obs.mu.Unlock()
	total := 0
	for _, c := range obs.counts {
		total += c
	}
	if total != 4 {
		t.Fatalf("ObserveDrained total = %d, want 4 (parked message must not be counted)", total)
	}
}
