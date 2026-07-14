package sequence_test

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"strings"
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

func TestNewRelayRejectsZeroPollInterval(t *testing.T) {
	// A zero PollInterval previously panicked inside time.NewTicker; it must
	// now be rejected at construction time. Valid store and sender, so no
	// earlier nil-guard can mask the guard under test; the message must name
	// the violated field.
	_, err := sequence.NewRelay("c", newFakeStore(), noopSender, sequence.WithPollInterval(0))
	if err == nil || !strings.Contains(err.Error(), "PollInterval must be > 0") {
		t.Fatalf("err = %v, want the PollInterval > 0 validation error", err)
	}
}

func TestNewRelayRejectsZeroBatchSize(t *testing.T) {
	_, err := sequence.NewRelay("c", newFakeStore(), noopSender, sequence.WithBatchSize(0))
	if err == nil || !strings.Contains(err.Error(), "sequence: BatchSize") {
		t.Fatalf("err = %v, want the BatchSize > 0 validation error", err)
	}
}

func TestNewRelayRejectsZeroSequenceBatchSize(t *testing.T) {
	_, err := sequence.NewRelay("c", newFakeStore(), noopSender, sequence.WithSequenceBatchSize(0))
	if err == nil || !strings.Contains(err.Error(), "SequenceBatchSize") {
		t.Fatalf("err = %v, want the SequenceBatchSize > 0 validation error", err)
	}
}

func TestNewRelayRejectsZeroLeaseTTL(t *testing.T) {
	_, err := sequence.NewRelay("c", newFakeStore(), noopSender, sequence.WithLeaseTTL(0))
	if err == nil || !strings.Contains(err.Error(), "LeaseTTL must be > 0") {
		t.Fatalf("err = %v, want the LeaseTTL > 0 validation error", err)
	}
}

func TestNewRelayRejectsRetentionWindowWithoutSweepCadence(t *testing.T) {
	// A positive RetentionWindow with a zero sweep cadence/batch would make
	// retention silently non-functional (maybeSweep's `<= 0` guard always
	// skips it), so it must be rejected at construction time.
	_, err := sequence.NewRelay("c", newFakeStore(), noopSender, sequence.WithRetention(24*time.Hour, 0, 0))
	if err == nil || !strings.Contains(err.Error(), "RetentionWindow") {
		t.Fatalf("err = %v, want the RetentionWindow sweep-cadence validation error", err)
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

	seqCalls  int // number of SequenceMessages invocations, for loop-count assertions
	listCalls int // number of ListMessages invocations, for did-drain-run assertions

	// Recorded by CommitOffset, so tests can assert the final commit on a
	// shutdown path goes through a fresh bounded context (deadline set, not
	// already canceled) instead of the dead run ctx.
	commitHadDeadline bool
	commitCtxErr      error

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
	for i := range n {
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
	s.listCalls++
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

func (s *fakeStore) CommitOffset(ctx context.Context, name string, seq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, s.commitHadDeadline = ctx.Deadline()
	s.commitCtxErr = ctx.Err()
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

// snapshotListCalls reads the ListMessages invocation count under mu.
func (s *fakeStore) snapshotListCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listCalls
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

// TestSendFailureStopsLaneEvenWithErrorHandler pins the ErrorHandler's scope:
// it parks POISON rows only, never send failures. A send failure is
// downstream trouble (broker down), and parking healthy messages during an
// outage would bulk-divert the backlog to the DLQ while permanently advancing
// the offset past it. The lane must stop at the failure and retry it next
// tick — order and delivery preserved.
func TestSendFailureStopsLaneEvenWithErrorHandler(t *testing.T) {
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
	if len(parked) != 0 {
		t.Fatalf("parked = %v, want none (send failures must never be parked)", parked)
	}
	if off := st.offsets["c"]; off != 2 {
		t.Fatalf("offset = %d, want 2 (lane stops at the send failure)", off)
	}
	if len(got) != 2 {
		t.Fatalf("delivered = %v, want [1 2]", got)
	}

	// While downstream stays broken, subsequent ticks keep stopping at seq 3 —
	// nothing is skipped and nothing reaches the DLQ.
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce[retry]: %v", err)
	}
	if off := st.offsets["c"]; off != 2 {
		t.Fatalf("offset = %d, want 2 (still stopped while downstream is down)", off)
	}
	if len(parked) != 0 {
		t.Fatalf("parked = %v after retry tick, want none", parked)
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

// TestMaybeSweepRunsOnInterval proves the retention sweep cadence is
// wall-clock time (RetentionSweepInterval), decoupled from PollInterval: the
// first leader tick sweeps immediately, ticks within the interval skip, and a
// tick after the interval elapses sweeps again — with a `before` cutoff of
// now-minus-window. Uses synctest's fake clock for deterministic elapsing.
func TestMaybeSweepRunsOnInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := newFakeStore()
		window := 24 * time.Hour
		interval := 5 * time.Minute
		r, err := sequence.NewRelay("c", st, noopSender, sequence.WithRetention(window, interval, 100))
		if err != nil {
			t.Fatalf("NewRelay: %v", err)
		}

		// First tick sweeps immediately; two more inside the interval skip.
		for i := range 3 {
			if err := r.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce[%d]: %v", i, err)
			}
		}
		if calls, before := st.snapshotSweep(); calls != 1 {
			t.Fatalf("sweepCalls = %d within the interval, want 1", calls)
		} else if diff := time.Now().Add(-window).Sub(before); diff < -time.Minute || diff > time.Minute {
			t.Fatalf("sweep `before` = %v, want ~now-window", before)
		}

		// After the interval elapses, the next tick sweeps again.
		time.Sleep(interval + time.Second)
		if err := r.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce[after interval]: %v", err)
		}
		if calls, _ := st.snapshotSweep(); calls != 2 {
			t.Fatalf("sweepCalls = %d after the interval elapsed, want 2", calls)
		}
	})
}

// TestMaybeSweepNoopWithoutRetentionStore proves a store lacking
// sequence.RetentionStore never gets a SweepMessages call and RunOnce does
// not panic, even with RetentionWindow configured via a capable neighbor.
func TestMaybeSweepNoopWithoutRetentionStore(t *testing.T) {
	inner := newFakeStore()
	st := noRetentionStore{inner: inner}
	r, err := sequence.NewRelay("c", st, noopSender, sequence.WithRetention(time.Hour, time.Minute, 100))
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
	r, err := sequence.NewRelay("c", st, noopSender, sequence.WithRetention(time.Hour, time.Minute, 100))
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

// TestNewRelayRejectsOverlongName pins the maxNameLen guard: the reference
// schema keys outbox_offsets/relay_locks/outbox_sequencers on VARCHAR(64), and
// under a relaxed sql_mode a longer name would be silently truncated into
// another group's offset row and leader lock. Same guard for an overridden
// LeaderLockName.
func TestNewRelayRejectsOverlongName(t *testing.T) {
	long := strings.Repeat("g", 65)
	if _, err := sequence.NewRelay(long, newFakeStore(), noopSender); err == nil || !strings.Contains(err.Error(), "64") {
		t.Fatalf("err = %v, want the 64-byte name guard", err)
	}
	if _, err := sequence.NewRelay("c", newFakeStore(), noopSender, sequence.WithLeaderLockName(long)); err == nil || !strings.Contains(err.Error(), "64") {
		t.Fatalf("err = %v, want the 64-byte LeaderLockName guard", err)
	}
	// 64 bytes exactly is the schema's limit and must be accepted.
	if _, err := sequence.NewRelay(strings.Repeat("g", 64), newFakeStore(), noopSender); err != nil {
		t.Fatalf("64-byte name rejected: %v", err)
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

// TestLeadershipLostDuringSequenceSkipsDrainAndSweep proves losing leadership
// at the sequencer's between-pages renewal stops the WHOLE pass, not just the
// sequencer loop: a known non-leader must not go on to drain (a full-page
// duplicate burst against the new leader) or sweep. The pass still ends
// cleanly — losing leadership is not an error.
func TestLeadershipLostDuringSequenceSkipsDrainAndSweep(t *testing.T) {
	st := &revokingLeaderStore{fakeStore: newFakeStore()}
	st.allowed.Store(1) // RunOnce's opening acquire succeeds; the sequencer's renewal fails
	for range 4 {
		st.append(msg(0))
	}
	var sent int
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { sent++; return nil })

	r, err := sequence.NewRelay("c", st, sender,
		sequence.WithSequenceBatchSize(2), sequence.WithStartFromBeginning(),
		sequence.WithRetention(time.Hour, time.Minute, 100))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if runErr := r.RunOnce(t.Context()); runErr != nil {
		t.Fatalf("RunOnce = %v, want nil (losing leadership is not an error)", runErr)
	}
	if sent != 0 {
		t.Fatalf("sent = %d, want 0 (drain must not run as a known non-leader)", sent)
	}
	if calls := st.snapshotListCalls(); calls != 0 {
		t.Fatalf("ListMessages called %d times, want 0 (drain must not run as a known non-leader)", calls)
	}
	if calls, _ := st.snapshotSweep(); calls != 0 {
		t.Fatalf("sweepCalls = %d, want 0 (sweep must not run as a known non-leader)", calls)
	}
}

// erroringLeaderStore wraps fakeStore's leader lock but makes every
// TryAcquireLeaderLock call numbered >= failFrom return err, simulating a
// leader store that starts failing mid-pass and stays down.
type erroringLeaderStore struct {
	*fakeStore
	calls    atomic.Int32
	failFrom int32
	err      error
}

func (s *erroringLeaderStore) TryAcquireLeaderLock(ctx context.Context, name, holderID string, ttl time.Duration) (bool, error) {
	if s.calls.Add(1) >= s.failFrom {
		return false, s.err
	}
	return s.fakeStore.TryAcquireLeaderLock(ctx, name, holderID, ttl)
}

// TestRenewalErrorMidDrainEndsPassCleanly proves a between-pages renewal that
// fails with an I/O ERROR (not just a lost lock) takes the same route as a
// lost lease: the pass ends cleanly with what is already committed and no
// further pages are drained. The persistent store error is not swallowed for
// good — the next tick's opening TryAcquire surfaces it.
func TestRenewalErrorMidDrainEndsPassCleanly(t *testing.T) {
	sentinel := errors.New("lock store boom")
	st := &erroringLeaderStore{fakeStore: newFakeStore(), failFrom: 2, err: sentinel}
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
	// First pass: the opening acquire (call 1) succeeds; the renewal after
	// the first full page (call 2) errors — clean stop, one page committed.
	if runErr := r.RunOnce(t.Context()); runErr != nil {
		t.Fatalf("RunOnce = %v, want nil (a renewal error ends the pass cleanly)", runErr)
	}
	if sent != 2 {
		t.Fatalf("sent = %d, want 2 (no further pages after the renewal error)", sent)
	}
	if off := st.offsets["c"]; off != 2 {
		t.Fatalf("offset = %d, want 2 (first page committed before the pass ended)", off)
	}
	// Next tick: the opening TryAcquire (call 3) surfaces the persistent
	// leader-store error.
	if runErr := r.RunOnce(t.Context()); !errors.Is(runErr, sentinel) {
		t.Fatalf("next RunOnce err = %v, want %v (opening TryAcquire must surface the store error)", runErr, sentinel)
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
	// The final commit runs after the run ctx is already dead: it must go
	// through commitOffset's fresh bounded context (a real store would fail
	// the write on the canceled ctx and redeliver the acknowledged sends).
	if st.commitCtxErr != nil {
		t.Fatalf("final commit ctx already dead (%v), want a live fresh context", st.commitCtxErr)
	}
	if !st.commitHadDeadline {
		t.Fatal("final commit ctx had no deadline, want a bounded fresh context")
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
	if len(parked) != 1 || len(parkedErrs) != 1 || parked[0].Seq != 3 {
		t.Fatalf("parked = %+v (errs %v), want exactly the seq-3 poison row once", parked, parkedErrs)
	}
	de, ok := errors.AsType[*sequence.DecodeError](parkedErrs[0])
	if !ok || de.Seq != 3 {
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
	de, ok := errors.AsType[*sequence.DecodeError](runErr)
	if !ok || de.Seq != 3 {
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
// ObserveDrained's count is successful sends only — a parked poison row is
// surfaced via ObserveError and not counted.
func TestObserveDrainedExcludesParkedMessages(t *testing.T) {
	st := newFakeStore()
	for range 5 {
		st.append(msg(0))
	}
	st.poisonSeq = 3
	obs := &drainCountObserver{}
	r, err := sequence.NewRelay("c", st, noopSender, sequence.WithStartFromBeginning(),
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
		t.Fatalf("ObserveDrained total = %d, want 4 (parked poison row must not be counted)", total)
	}
}

// TestSequenceLoopBoundedPerTick pins the maxPagesPerTick fairness cap on the
// sequencer loop: a backlog that keeps every page full must not pin the pass
// in sequence() forever (starving drain and the sweep). One tick sequences at
// most 64 pages; the next tick continues.
func TestSequenceLoopBoundedPerTick(t *testing.T) {
	st := newFakeStore()
	for range 70 {
		st.append(msg(0))
	}
	r, err := sequence.NewRelay("c", st, noopSender,
		sequence.WithSequenceBatchSize(1), sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if calls := st.snapshotSeqCalls(); calls != 64 {
		t.Fatalf("SequenceMessages called %d times in one tick, want 64 (the per-tick cap)", calls)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce[2]: %v", err)
	}
	if len(st.log) != 70 {
		t.Fatalf("sequenced %d after two ticks, want 70 (the next tick continues)", len(st.log))
	}
}

// TestDrainLoopBoundedPerTick pins the same cap on the drain loop: at most 64
// full pages per tick, with the remainder delivered next tick.
func TestDrainLoopBoundedPerTick(t *testing.T) {
	st := newFakeStore()
	for range 70 {
		st.append(msg(0))
	}
	var sent int
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { sent++; return nil })
	r, err := sequence.NewRelay("c", st, sender,
		sequence.WithBatchSize(1), sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sent != 64 {
		t.Fatalf("sent = %d in one tick, want 64 (the per-tick cap)", sent)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce[2]: %v", err)
	}
	if sent != 70 {
		t.Fatalf("sent = %d after two ticks, want 70", sent)
	}
	if off := st.offsets["c"]; off != 70 {
		t.Fatalf("offset = %d, want 70", off)
	}
}

// fullDrainObserver records every ObserveDrained call's count, age, and more.
type fullDrainObserver struct {
	mu     sync.Mutex
	counts []int
	ages   []time.Duration
	mores  []bool
}

func (o *fullDrainObserver) ObserveDrained(_ string, count int, age time.Duration, more bool) {
	o.mu.Lock()
	o.counts = append(o.counts, count)
	o.ages = append(o.ages, age)
	o.mores = append(o.mores, more)
	o.mu.Unlock()
}
func (o *fullDrainObserver) ObserveSequenced(string, int) {}
func (o *fullDrainObserver) ObserveError(string, error)   {}

// TestPoisonOnlyPageObservesDrained proves a page whose decoded prefix is
// empty but whose poison row is parked still reports ObserveDrained: the pass
// disposed of a message, so it must not be invisible to the lag/throughput
// signal. With no decoded row to anchor the lag on, oldestAge is zero; more
// is true (rows may follow the parked poison).
func TestPoisonOnlyPageObservesDrained(t *testing.T) {
	st := newFakeStore()
	st.append(msg(0))
	st.poisonSeq = 1

	obs := &fullDrainObserver{}
	r, err := sequence.NewRelay("c", st, noopSender, sequence.WithStartFromBeginning(),
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
	if len(obs.counts) != 1 {
		t.Fatalf("ObserveDrained called %d times, want 1 (the poison-only page must be observed; the trailing empty page must not)", len(obs.counts))
	}
	if obs.counts[0] != 0 {
		t.Fatalf("ObserveDrained count = %d, want 0 (the parked poison is not a successful send)", obs.counts[0])
	}
	if obs.ages[0] != 0 {
		t.Fatalf("ObserveDrained oldestAge = %v, want 0 (no decoded row to anchor the lag on)", obs.ages[0])
	}
	if !obs.mores[0] {
		t.Fatal("ObserveDrained more = false, want true (rows may follow the parked poison)")
	}
	if off := st.offsets["c"]; off != 1 {
		t.Fatalf("offset = %d, want 1 (advanced past the parked poison row)", off)
	}
}
