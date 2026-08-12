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
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
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

// TestNewRelayRejectsZeroSweepCadence pins that an explicitly configured sweep
// must be able to run: a zero interval or batch makes retention silently
// non-functional (maybeSweep's `<= 0` guard always skips it), so it is rejected
// at construction time. WithoutRetention() is the spelling for "no sweep here".
//
// (This is what remains of the old window-plus-cadence validation. The window
// half moved to the store — see the TiDB store's WithRetentionWindow, which
// rejects a non-positive window the same way.)
func TestNewRelayRejectsZeroSweepCadence(t *testing.T) {
	_, err := sequence.NewRelay("c", newFakeStore(), noopSender, sequence.WithRetention(0, 0))
	if err == nil || !strings.Contains(err.Error(), "RetentionSweepInterval") {
		t.Fatalf("err = %v, want the sweep-cadence validation error", err)
	}
	// The remedy the message names must actually work.
	if _, err := sequence.NewRelay("c", newFakeStore(), noopSender, sequence.WithoutRetention()); err != nil {
		t.Fatalf("NewRelay with WithoutRetention: %v", err)
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
	// commitErrAboveSeq scopes commitErr to seq > n (0 = every commit fails), so a
	// test can fail watermark advances while letting the seq-0 registration commit
	// through. See failCommitsAbove.
	commitErrAboveSeq int64

	// poisonSeq, when non-zero, marks that row as undecodable: ListMessages
	// returns the decoded prefix before it plus a *sequence.DecodeError, per
	// the Store contract.
	poisonSeq int64

	seqCalls    int // number of SequenceMessages invocations, for loop-count assertions
	listCalls   int // number of ListMessages invocations, for did-drain-run assertions
	initCalls   int // number of InitOffsetLatest invocations (priming churn assertions)
	commitCalls int // number of CommitOffset invocations (priming churn assertions)

	// Recorded by CommitOffset, so tests can assert the final commit on a
	// shutdown path goes through a fresh bounded context (deadline set, not
	// already canceled) instead of the dead run ctx.
	commitHadDeadline bool
	commitCtxErr      error

	sweepErr   error
	sweepCalls int
	// lastSweepLimit is the page bound the relay asked for. It replaced a
	// recorded `olderThan` window: the retention WINDOW is now the store's own
	// property (see sequence.Sweeper), so the only sweep parameter the relay
	// still contributes — and therefore the only one a relay test can pin — is
	// the cadence's batch size.
	lastSweepLimit int
	sweepBacklog   int // rows the fake pretends are deletable; each pass drains up to limit
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

func (s *fakeStore) Offset(_ context.Context, name string) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.offsetErr != nil {
		return 0, false, s.offsetErr
	}
	off, exists := s.offsets[name]
	return off, exists, nil
}

// CommitOffset mirrors the SQL store's INSERT ... ON DUPLICATE KEY UPDATE
// last_seq = GREATEST(...): monotone, and insert-if-absent. The insert half
// matters — the relay REGISTERS a WithStartFromBeginning group by committing 0,
// so that the sweep's MIN(last_seq) cutoff accounts for it before it has
// delivered anything. A fake that only wrote on seq > current would model an
// UPDATE-only store and hide that.
func (s *fakeStore) CommitOffset(ctx context.Context, name string, seq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commitCalls++
	_, s.commitHadDeadline = ctx.Deadline()
	s.commitCtxErr = ctx.Err()
	if s.commitErr != nil && seq > s.commitErrAboveSeq {
		return s.commitErr
	}
	cur, exists := s.offsets[name]
	if !exists || seq > cur { // insert-if-absent, then GREATEST
		s.offsets[name] = seq
	}
	return nil
}

// failCommitsAbove makes CommitOffset fail only for seq > n, so a test can let the
// WithStartFromBeginning registration commit (seq 0) succeed while every real
// watermark advance fails.
func (s *fakeStore) failCommitsAbove(n int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commitErr = err
	s.commitErrAboveSeq = n
}

// hasOffsetRow reports whether the named group has an offset row at all — the
// thing the retention sweep's MIN(last_seq) is computed over.
func (s *fakeStore) hasOffsetRow(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.offsets[name]
	return ok
}

// InitOffsetLatest mirrors the SQL store's insert-if-absent: create name's
// offset row at the current max sequenced seq ONLY if no row exists, and
// return the effective committed offset. An existing row — even at 0 — is a
// committed position and is never modified.
func (s *fakeStore) InitOffsetLatest(_ context.Context, name string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initCalls++
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

// SweepMessages implements sequence.Sweeper: it records invocation count and
// the page bound, and drains a scripted deletable backlog up to `limit` per
// pass (for the loop-while-full assertions).
//
// There is no cutoff parameter to record: the retention WINDOW is the store's
// own property now, because the sweep's cutoff is MIN(last_seq) across all
// consumer groups and therefore store-wide, while a relay is per-group. A fake
// that still took a window would be modeling a contract the runtime no longer
// has.
func (s *fakeStore) SweepMessages(_ context.Context, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepCalls++
	s.lastSweepLimit = limit
	if s.sweepErr != nil {
		return 0, s.sweepErr
	}
	n := min(limit, s.sweepBacklog)
	s.sweepBacklog -= n
	return n, nil
}

// snapshotSweep reads the sweep counters under mu.
func (s *fakeStore) snapshotSweep() (calls, limit int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sweepCalls, s.lastSweepLimit
}

// snapshotInitCalls reads the InitOffsetLatest invocation count under mu.
func (s *fakeStore) snapshotInitCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initCalls
}

// snapshotCommitCalls reads the CommitOffset invocation count under mu.
func (s *fakeStore) snapshotCommitCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitCalls
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

// msg builds an unsequenced pending row (Seq 0 until the fake sequencer runs).
func msg() *outbox.Message {
	return &outbox.Message{ID: "id", Metadata: event.NewMetadata("t"), CreateTime: time.Now()}
}

// --- runtime tests ----------------------------------------------------------

func TestRunOnceSequencesThenDrainsSameTick(t *testing.T) {
	st := newFakeStore()
	st.append(msg())
	st.append(msg())
	st.append(msg())

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
		st.append(msg())
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
		st.append(msg())
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
	st.append(msg())
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

// TestSendFailureStopsLaneEvenWithPoisonHandler pins the PoisonHandler's scope:
// it parks POISON rows only, never send failures. A send failure is
// downstream trouble (broker down), and parking healthy messages during an
// outage would bulk-divert the backlog to the DLQ while permanently advancing
// the offset past it. The lane must stop at the failure and retry it next
// tick — order and delivery preserved.
func TestSendFailureStopsLaneEvenWithPoisonHandler(t *testing.T) {
	st := newFakeStore()
	for range 5 {
		st.append(msg())
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
	r, err := sequence.NewRelay("c", st, sender, sequence.WithStartFromBeginning(), sequence.WithPoisonHandler(
		func(_ context.Context, m *outbox.Message, _ error) error { parked = append(parked, m.Seq); return nil },
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

// recordingObserver tracks whether OnError was ever called.
type recordingObserver struct {
	mu          sync.Mutex
	errObserved bool
}

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
	r, err := sequence.NewRelay("c", st, sender,
		sequence.WithObserver(relay.Observer{OnError: obs.ObserveError}), sequence.WithPollInterval(time.Millisecond))
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

	r, err := sequence.NewRelay("c", st, sender,
		sequence.WithObserver(relay.Observer{OnError: obs.ObserveError}), sequence.WithPollInterval(5*time.Millisecond))
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
		st.append(msg())
	}
	if _, err := st.SequenceMessages(t.Context(), 100); err != nil {
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

	st.append(msg())
	st.append(msg())
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(got) != 2 || got[0] != 4 || got[1] != 5 {
		t.Fatalf("delivered = %v, want [4 5] (only events sequenced after the group started)", got)
	}
}

// TestStartFromBeginningRegistersOffsetRow pins the retention-protection half of
// WithStartFromBeginning. Sweep protection is derived from MIN(last_seq) over the
// EXISTING offset rows, so a replaying group that has no row of its own is
// invisible to the cutoff: the sweep computes it from the other groups alone and
// can delete the very history this group was configured to replay. Skipping
// InitOffsetLatest (correct — it would jump the group to latest) must therefore
// still register the row, by committing 0.
//
// The downstream sender fails here on purpose: that is the dangerous case, since
// nothing is ever committed through the normal path, so the row can only exist if
// registration is explicit.
func TestStartFromBeginningRegistersOffsetRow(t *testing.T) {
	st := newFakeStore()
	st.append(msg())
	st.append(msg())
	if _, err := st.SequenceMessages(t.Context(), 10); err != nil {
		t.Fatalf("seed SequenceMessages: %v", err)
	}

	// A pre-existing group sitting above the replay range: without a row for the
	// replaying group, MIN(last_seq) would be computed from this one alone.
	st.offsets["established"] = 2

	sendErr := errors.New("broker down")
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return sendErr })

	r, err := sequence.NewRelay("replayer", st, sender, sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if !st.hasOffsetRow("replayer") {
		t.Fatal("no offset row for the StartFromBeginning group: the retention sweep's MIN(last_seq) cannot see it and may delete the log it is replaying")
	}
	if off := st.offsets["replayer"]; off != 0 {
		t.Fatalf("offset = %d, want 0 (registration must not skip any of the retained log)", off)
	}
}

// TestStartFromBeginningStillReplaysWholeLog pins that the registration commit
// above did not cost the group any history.
func TestStartFromBeginningStillReplaysWholeLog(t *testing.T) {
	st := newFakeStore()
	st.append(msg())
	st.append(msg())
	st.append(msg())
	if _, err := st.SequenceMessages(t.Context(), 10); err != nil {
		t.Fatalf("seed SequenceMessages: %v", err)
	}

	var got []int64
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		got = append(got, mdSeq(md))
		return nil
	})

	r, err := sequence.NewRelay("replayer", st, sender, sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("delivered = %v, want [1 2 3] (the whole retained log)", got)
	}
}

// TestUnsendableClassifierParksAndAdvances pins the escape hatch for a message
// the broker will never accept. Stopping the lane on every send failure is right
// for an outage but a trap for a permanently-unsendable row: it sits at the head
// of the log and every event behind it stops being delivered indefinitely,
// recoverable only by hand-editing offsets in a live database (v1's
// WithErrorHandler moved past such a row).
func TestUnsendableClassifierParksAndAdvances(t *testing.T) {
	st := newFakeStore()
	st.append(msg())
	st.append(msg())
	st.append(msg())

	unsendable := errors.New("body exceeds frame_max")
	var delivered []int64
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		if mdSeq(md) == 2 {
			return unsendable
		}
		delivered = append(delivered, mdSeq(md))
		return nil
	})

	var parked []string
	r, err := sequence.NewRelay("c", st, sender,
		sequence.WithStartFromBeginning(),
		sequence.WithPoisonHandler(func(_ context.Context, m *outbox.Message, _ error) error {
			parked = append(parked, m.Metadata.ID)
			return nil
		}),
		sequence.WithUnsendableClassifier(func(err error) bool { return errors.Is(err, unsendable) }),
	)
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(parked) != 1 || parked[0] != "2" {
		t.Fatalf("parked = %v, want [2]", parked)
	}
	if len(delivered) != 2 || delivered[0] != 1 || delivered[1] != 3 {
		t.Fatalf("delivered = %v, want [1 3] (the lane must advance past the unsendable row)", delivered)
	}
	if off := st.offsets["c"]; off != 3 {
		t.Fatalf("offset = %d, want 3 (committed past the parked row)", off)
	}
}

// TestUnsendableClassifierStopsLaneOnUnconfirmedPark pins that advancing past an
// unsendable row requires a CONFIRMED park: committing the offset past it is
// irreversible, so an unconfirmed DLQ write must leave the row for the next tick
// rather than skip the event forever.
func TestUnsendableClassifierStopsLaneOnUnconfirmedPark(t *testing.T) {
	st := newFakeStore()
	st.append(msg())
	st.append(msg())

	unsendable := errors.New("body exceeds frame_max")
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		if mdSeq(md) == 1 {
			return unsendable
		}
		return nil
	})

	r, err := sequence.NewRelay("c", st, sender,
		sequence.WithStartFromBeginning(),
		sequence.WithPoisonHandler(func(context.Context, *outbox.Message, error) error {
			return errors.New("dlq unavailable")
		}),
		sequence.WithUnsendableClassifier(func(err error) bool { return errors.Is(err, unsendable) }),
	)
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if off := st.offsets["c"]; off != 0 {
		t.Fatalf("offset = %d, want 0 (an unconfirmed park must not advance past the row)", off)
	}
}

// TestUnsendableClassifierLeavesOtherFailuresStoppingTheLane pins the narrowness
// of the hatch: a failure the classifier does not claim (an outage) still stops
// the lane, so a broker blip cannot bulk-divert the backlog to the DLQ.
func TestUnsendableClassifierLeavesOtherFailuresStoppingTheLane(t *testing.T) {
	st := newFakeStore()
	st.append(msg())
	st.append(msg())

	unsendable := errors.New("body exceeds frame_max")
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error {
		return errors.New("connection reset")
	})

	var parked int
	r, err := sequence.NewRelay("c", st, sender,
		sequence.WithStartFromBeginning(),
		sequence.WithPoisonHandler(func(context.Context, *outbox.Message, error) error {
			parked++
			return nil
		}),
		sequence.WithUnsendableClassifier(func(err error) bool { return errors.Is(err, unsendable) }),
	)
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if parked != 0 {
		t.Fatalf("parked = %d during an outage, want 0 (only classified-unsendable messages are parked)", parked)
	}
	if off := st.offsets["c"]; off != 0 {
		t.Fatalf("offset = %d, want 0 (the lane must stop on an unclassified failure)", off)
	}
}

// TestStuckLaneEscalatesAfterThreshold pins the wedge alarm. Stopping the lane on
// a send failure is right for an outage, but tick by tick it is indistinguishable
// from a message the broker will NEVER accept — and in the latter case every event
// behind it stops being delivered indefinitely, recoverable only by editing
// offsets in a live database. The per-tick OnError says only "a send failed", the
// same thing it says during a two-minute blip, so a distinct once-per-episode
// signal is what makes the wedge actionable.
func TestStuckLaneEscalatesAfterThreshold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := newFakeStore()
		st.append(msg())
		st.append(msg())

		sender := senderFunc(func(context.Context, *event.Metadata, []byte) error {
			return errors.New("broker refuses this message")
		})

		var stuck []*sequence.StuckLaneError
		obs := relay.Observer{OnError: func(_ string, err error) {
			if sl, ok := errors.AsType[*sequence.StuckLaneError](err); ok {
				stuck = append(stuck, sl)
			}
		}}

		r, err := sequence.NewRelay("c", st, sender,
			sequence.WithStartFromBeginning(), sequence.WithObserver(obs))
		if err != nil {
			t.Fatalf("NewRelay: %v", err)
		}

		// Well inside the threshold: a transient outage must not escalate.
		for range 3 {
			if err := r.RunOnce(t.Context()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
		}
		if len(stuck) != 0 {
			t.Fatalf("escalated %d times during a short outage, want 0", len(stuck))
		}

		// Past the threshold, still stuck on the same seq.
		time.Sleep(20 * time.Minute)
		synctest.Wait()

		for range 3 {
			if err := r.RunOnce(t.Context()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
		}

		if len(stuck) != 1 {
			t.Fatalf("escalated %d times, want exactly 1 (once per episode, not once per tick)", len(stuck))
		}
		if stuck[0].Position != "seq 1" {
			t.Fatalf("StuckLaneError.Position = %q, want \"seq 1\"", stuck[0].Position)
		}
		if stuck[0].StuckFor < 20*time.Minute {
			t.Fatalf("StuckLaneError.StuckFor = %v, want >= 20m", stuck[0].StuckFor)
		}
	})
}

// TestStuckLaneEscalatesOnUnconfirmedParks pins that EVERY path which wedges the
// lane escalates, not just the plain send failure. A message that can neither be
// sent nor parked (an unsendable body plus a broken DLQ), and a poison row that
// cannot be parked, stop the lane exactly as hard — and each of those paths breaks
// out of the loop separately, so each needs its own report. A path that stops
// without reporting leaves the wedge with nothing but the generic per-tick error,
// which is the gap the escalation exists to close.
func TestStuckLaneEscalatesOnUnconfirmedParks(t *testing.T) {
	dlqDown := func(context.Context, *outbox.Message, error) error {
		return errors.New("dlq unavailable")
	}

	cases := map[string]struct {
		setup []sequence.Option
		store func() *fakeStore
	}{
		"unsendable message, park fails": {
			setup: []sequence.Option{
				sequence.WithPoisonHandler(dlqDown),
				sequence.WithUnsendableClassifier(func(error) bool { return true }),
			},
			store: func() *fakeStore {
				st := newFakeStore()
				st.append(msg())
				return st
			},
		},
		"poison row, park fails": {
			setup: []sequence.Option{sequence.WithPoisonHandler(dlqDown)},
			store: func() *fakeStore {
				st := newFakeStore()
				st.append(msg())
				st.append(msg())
				return st
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st := tc.store()
				if name == "poison row, park fails" {
					if _, err := st.SequenceMessages(t.Context(), 10); err != nil {
						t.Fatalf("seed: %v", err)
					}
					st.poisonSeq = 1
				}

				sender := senderFunc(func(context.Context, *event.Metadata, []byte) error {
					return errors.New("broker refuses this message")
				})

				var stuck []*sequence.StuckLaneError
				opts := append([]sequence.Option{
					sequence.WithStartFromBeginning(),
					sequence.WithObserver(relay.Observer{OnError: func(_ string, err error) {
						if sl, ok := errors.AsType[*sequence.StuckLaneError](err); ok {
							stuck = append(stuck, sl)
						}
					}}),
				}, tc.setup...)

				r, err := sequence.NewRelay("c", st, sender, opts...)
				if err != nil {
					t.Fatalf("NewRelay: %v", err)
				}

				// One pass to arm the tracking, then past the threshold.
				_ = r.RunOnce(t.Context())
				time.Sleep(20 * time.Minute)
				synctest.Wait()
				_ = r.RunOnce(t.Context())
				_ = r.RunOnce(t.Context())

				if len(stuck) != 1 {
					t.Fatalf("escalated %d times, want exactly 1 (this stop path must report the wedge, once per episode)", len(stuck))
				}
				if stuck[0].StuckFor < 20*time.Minute {
					t.Fatalf("StuckLaneError.StuckFor = %v, want >= 20m", stuck[0].StuckFor)
				}
			})
		})
	}
}

// TestStuckLaneEscalatesDefaultPoisonWedge pins the escalation for the wedge that
// needs it most: a poison row with NO PoisonHandler, which is the default
// configuration. Retrying an undecodable row can never succeed, so the lane stops
// there permanently and nothing behind it is ever delivered — yet the only
// telemetry used to be the same per-tick decode error a transient condition
// produces. The park branch cannot report this case (it is gated on a handler
// being configured), so the terminal stop path has to.
func TestStuckLaneEscalatesDefaultPoisonWedge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := newFakeStore()
		st.append(msg())
		st.append(msg())
		if _, err := st.SequenceMessages(t.Context(), 10); err != nil {
			t.Fatalf("seed: %v", err)
		}
		st.poisonSeq = 1 // the head row is undecodable

		var stuck []*sequence.StuckLaneError
		obs := relay.Observer{OnError: func(_ string, err error) {
			if sl, ok := errors.AsType[*sequence.StuckLaneError](err); ok {
				stuck = append(stuck, sl)
			}
		}}

		// No WithPoisonHandler: the documented default.
		r, err := sequence.NewRelay("c", st, noopSender,
			sequence.WithStartFromBeginning(), sequence.WithObserver(obs))
		if err != nil {
			t.Fatalf("NewRelay: %v", err)
		}

		_ = r.RunOnce(t.Context())
		if len(stuck) != 0 {
			t.Fatalf("escalated %d times immediately, want 0", len(stuck))
		}

		time.Sleep(20 * time.Minute)
		synctest.Wait()
		_ = r.RunOnce(t.Context())
		_ = r.RunOnce(t.Context())

		if len(stuck) != 1 {
			t.Fatalf("escalated %d times, want exactly 1 (the default poison wedge must escalate, once per episode)", len(stuck))
		}
		if stuck[0].Position != "seq 1" {
			t.Fatalf("StuckLaneError.Position = %q, want \"seq 1\"", stuck[0].Position)
		}
	})
}

// TestStuckLaneEscalatesDespiteRedeliveredPrefix pins that progress on OTHER
// messages cannot reset the escalation timer.
//
// Whenever the offset commit fails, a pass re-delivers the prefix ahead of the
// wedged row before hitting it again. A tracker cleared by any successful disposal
// therefore restarts its timer on every single pass, and a permanently wedged lane
// never reaches the threshold — the escalation would be dead exactly in the
// scenario where a commit problem and a delivery problem coincide.
func TestStuckLaneEscalatesDespiteRedeliveredPrefix(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := newFakeStore()
		st.append(msg())
		st.append(msg())
		if _, err := st.SequenceMessages(t.Context(), 10); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// Watermark advances never land (the seq-0 registration commit still does),
		// so every pass re-reads from offset 0 and re-delivers seq 1 successfully
		// before wedging again on seq 2.
		st.failCommitsAbove(0, errors.New("commit unavailable"))

		sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
			if mdSeq(md) == 2 {
				return errors.New("broker refuses this message")
			}
			return nil
		})

		var stuck []*sequence.StuckLaneError
		obs := relay.Observer{OnError: func(_ string, err error) {
			if sl, ok := errors.AsType[*sequence.StuckLaneError](err); ok {
				stuck = append(stuck, sl)
			}
		}}

		r, err := sequence.NewRelay("c", st, sender,
			sequence.WithStartFromBeginning(), sequence.WithObserver(obs))
		if err != nil {
			t.Fatalf("NewRelay: %v", err)
		}

		// Many passes, each re-delivering seq 1 successfully before wedging on 2.
		for range 5 {
			_ = r.RunOnce(t.Context())
		}
		time.Sleep(20 * time.Minute)
		synctest.Wait()
		for range 5 {
			_ = r.RunOnce(t.Context())
		}

		if len(stuck) == 0 {
			t.Fatal("never escalated: a re-delivered prefix reset the stuck-lane timer every pass, so a permanent wedge stays invisible")
		}
		if stuck[0].Position != "seq 2" {
			t.Fatalf("StuckLaneError.Position = %q, want \"seq 2\"", stuck[0].Position)
		}
	})
}

// TestStuckLaneTrackingResetsOnProgress pins that the escalation follows a stuck
// LANE, not a cumulative failure count: once the relay makes progress the clock
// starts over, so a flaky downstream never accumulates its way to a false alarm.
func TestStuckLaneTrackingResetsOnProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := newFakeStore()
		st.append(msg())
		st.append(msg())

		fail := true
		sender := senderFunc(func(context.Context, *event.Metadata, []byte) error {
			if fail {
				return errors.New("transient")
			}
			return nil
		})

		var stuck int
		obs := relay.Observer{OnError: func(_ string, err error) {
			if _, ok := errors.AsType[*sequence.StuckLaneError](err); ok {
				stuck++
			}
		}}

		r, err := sequence.NewRelay("c", st, sender,
			sequence.WithStartFromBeginning(), sequence.WithObserver(obs))
		if err != nil {
			t.Fatalf("NewRelay: %v", err)
		}

		if err := r.RunOnce(t.Context()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}

		// Recover, then let far more than the threshold elapse.
		fail = false
		if err := r.RunOnce(t.Context()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if err := r.RunOnce(t.Context()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}

		if stuck != 0 {
			t.Fatalf("escalated %d times after the lane recovered, want 0", stuck)
		}
	})
}

// TestRetentionDefaultsOnAndIsWaivable pins the retention decision surface.
//
// Default-ON: the v2 log is never pruned on delivery, so a relay with no sweep
// grows outbox_messages until the cluster runs out of disk — months after the
// omission and nowhere near it. Defaulting cannot lose an event, since the sweep
// only deletes rows below EVERY group's committed offset.
//
// Waivable: the sweep's effect is store-wide (cutoff MIN(last_seq) across all
// groups) while a relay is per-group, so several relays sweeping one store is
// redundant rather than wrong — the extra passes just find less to do. A relay
// cannot see its siblings, so the escape is explicit: WithoutRetention on every
// relay but the one that owns the sweep. (How much history survives is no
// longer part of this decision at all — the window belongs to the store.)
func TestRetentionDefaultsOnAndIsWaivable(t *testing.T) {
	t.Run("on by default", func(t *testing.T) {
		st := newFakeStore()
		r, err := sequence.NewRelay("c", st, noopSender)
		if err != nil {
			t.Fatalf("NewRelay: %v", err)
		}
		if err := r.RunOnce(t.Context()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		calls, limit := st.snapshotSweep()
		if calls == 0 {
			t.Fatal("sweepCalls = 0 with default options, want the default sweep to run (an unswept log grows to disk-full)")
		}
		if limit != 1000 {
			t.Fatalf("sweep batch = %d, want the 1000-row default", limit)
		}
	})

	t.Run("WithoutRetention waives it", func(t *testing.T) {
		st := newFakeStore()
		r, err := sequence.NewRelay("c", st, noopSender, sequence.WithoutRetention())
		if err != nil {
			t.Fatalf("NewRelay: %v", err)
		}
		if err := r.RunOnce(t.Context()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if calls, _ := st.snapshotSweep(); calls != 0 {
			t.Fatalf("sweepCalls = %d after WithoutRetention, want 0", calls)
		}
	})

	t.Run("WithRetention retunes the cadence", func(t *testing.T) {
		st := newFakeStore()
		r, err := sequence.NewRelay("c", st, noopSender,
			sequence.WithRetention(time.Minute, 100))
		if err != nil {
			t.Fatalf("NewRelay: %v", err)
		}
		if err := r.RunOnce(t.Context()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		calls, limit := st.snapshotSweep()
		if calls == 0 {
			t.Fatal("sweepCalls = 0 despite an explicit WithRetention")
		}
		if limit != 100 {
			t.Fatalf("sweep batch = %d, want the configured 100", limit)
		}
	})

	t.Run("WithRetention and WithoutRetention conflict", func(t *testing.T) {
		_, err := sequence.NewRelay("c", newFakeStore(), noopSender,
			sequence.WithRetention(time.Minute, 100), sequence.WithoutRetention())
		if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("err = %v, want a mutually-exclusive error", err)
		}
	})

	t.Run("default window on a store without Sweeper is not an error", func(t *testing.T) {
		// A legitimate topology: the store prunes itself, or another relay owns the
		// sweep. Only an EXPLICIT WithRetention makes the capability mandatory.
		if _, err := sequence.NewRelay("c", storeWithoutSweeper{inner: newFakeStore()}, noopSender); err != nil {
			t.Fatalf("NewRelay: %v", err)
		}
	})
}

// TestSweepReportsZeroCounts pins that a blocked sweep is observable. The cutoff is
// MIN(last_seq) across ALL groups, so one lagging or replaying group pins it and
// blocks pruning store-wide. Gating OnSwept on n > 0 made a blocked sweep and a
// healthy idle one emit NOTHING alike, so the log could grow toward disk-full with
// no way to tell them apart.
func TestSweepReportsZeroCounts(t *testing.T) {
	st := newFakeStore() // sweepBacklog 0: nothing deletable
	var swept []int
	obs := relay.Observer{OnSwept: func(_ string, n int) { swept = append(swept, n) }}

	r, err := sequence.NewRelay("c", st, noopSender,
		sequence.WithRetention(time.Minute, 100), sequence.WithObserver(obs))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(swept) != 1 || swept[0] != 0 {
		t.Fatalf("OnSwept counts = %v, want exactly [0] (a sweep that deleted nothing must still report)", swept)
	}
}

// TestNewRelayRejectsUnsendableClassifierWithoutPoisonHandler pins the
// construction guard: there is nowhere to park an unsendable message without a
// PoisonHandler, so the combination would silently degrade back to
// stop-the-lane-forever.
func TestNewRelayRejectsUnsendableClassifierWithoutPoisonHandler(t *testing.T) {
	_, err := sequence.NewRelay("c", newFakeStore(), noopSender,
		sequence.WithUnsendableClassifier(func(error) bool { return true }))
	if err == nil || !strings.Contains(err.Error(), "WithPoisonHandler") {
		t.Fatalf("err = %v, want an error naming WithPoisonHandler", err)
	}
}

// clockStore decorates the fake with sequence.Clock, reporting a store clock the
// test controls independently of the host clock.
type clockStore struct {
	*fakeStore
	now  time.Time
	err  error
	call int
}

func (s *clockStore) StoreNow(context.Context) (time.Time, error) {
	s.call++
	if s.err != nil {
		return time.Time{}, s.err
	}
	return s.now, nil
}

// TestOldestAgeUsesStoreClock pins the lag metric's clock domain. CreateTime is
// stamped by the STORE, so measuring its age against the relay host's clock folds
// NTP skew between the two hosts into the metric — and on a pod whose clock
// trails the database it reports a NEGATIVE age for a genuinely stale backlog,
// which a gauge plots as ~0 so the lag alert never fires. Here the host clock is
// far ahead of the store's; the reported age must follow the store.
func TestOldestAgeUsesStoreClock(t *testing.T) {
	st := &clockStore{fakeStore: newFakeStore()}
	m := msg()
	m.CreateTime = time.Now().Add(-90 * time.Minute) // "store" insert time
	st.append(m)
	st.now = m.CreateTime.Add(30 * time.Minute) // store clock: 30m of real lag

	var ages []time.Duration
	obs := relay.Observer{OnDrained: func(_ string, _ int, oldestAge time.Duration, _ bool) {
		ages = append(ages, oldestAge)
	}}

	r, err := sequence.NewRelay("c", st, noopSender,
		sequence.WithStartFromBeginning(), sequence.WithObserver(obs))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(ages) != 1 {
		t.Fatalf("OnDrained fired %d times, want 1", len(ages))
	}
	if ages[0] != 30*time.Minute {
		t.Fatalf("oldestAge = %v, want 30m (measured against the store clock, not the host's ~90m)", ages[0])
	}
}

// TestOldestAgeSkipsStoreClockWithoutObserver pins the cost guard: the store
// clock is only worth a round trip when something consumes the value.
func TestOldestAgeSkipsStoreClockWithoutObserver(t *testing.T) {
	st := &clockStore{fakeStore: newFakeStore()}
	st.now = time.Now()
	st.append(msg())

	r, err := sequence.NewRelay("c", st, noopSender, sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if st.call != 0 {
		t.Fatalf("StoreNow called %d times with no OnDrained observer, want 0", st.call)
	}
}

// TestOldestAgeFallsBackWhenStoreClockFails pins that lag is telemetry: a failed
// clock read degrades to the host clock instead of failing the pass.
func TestOldestAgeFallsBackWhenStoreClockFails(t *testing.T) {
	st := &clockStore{fakeStore: newFakeStore(), err: errors.New("clock unavailable")}
	m := msg()
	m.CreateTime = time.Now().Add(-time.Hour)
	st.append(m)

	var ages []time.Duration
	obs := relay.Observer{OnDrained: func(_ string, _ int, oldestAge time.Duration, _ bool) {
		ages = append(ages, oldestAge)
	}}

	r, err := sequence.NewRelay("c", st, noopSender,
		sequence.WithStartFromBeginning(), sequence.WithObserver(obs))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(ages) != 1 || ages[0] < 59*time.Minute {
		t.Fatalf("oldestAge = %v, want ~1h from the host-clock fallback", ages)
	}
}

// TestDeletedOffsetRowRePrimesAtLatest pins that losing the offset row does not
// turn into a full replay.
//
// DeleteOffset is the documented way to decommission a retired group and unpin the
// retention sweep, and it can land on a group whose relay is still running (wrong
// name, or a decommission before the pod drained). The relay then reads offset 0
// again. Re-priming at latest makes that harmless; caching "already initialized"
// in the process instead makes it replay every retained row — seven days of events
// by default — to the downstream broker.
func TestDeletedOffsetRowRePrimesAtLatest(t *testing.T) {
	st := newFakeStore()
	st.append(msg())
	st.append(msg())
	st.append(msg())

	var got []int64
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		got = append(got, mdSeq(md))
		return nil
	})

	r, err := sequence.NewRelay("c", st, sender) // default: start at latest
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	// First pass primes at latest and delivers nothing from the existing log.
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("delivered %v on the priming pass, want nothing", got)
	}

	// The offset row is deleted underneath the running relay.
	delete(st.offsets, "c")

	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce after the row was deleted: %v", err)
	}

	if len(got) != 0 {
		t.Fatalf("delivered %v after the offset row was deleted, want nothing: "+
			"the relay must re-prime at latest, not replay the retained log", got)
	}
	if !st.hasOffsetRow("c") {
		t.Fatal("offset row was not recreated after deletion")
	}
}

// TestOffsetPrimingHappensOnceWhileTheRowExists pins that priming is keyed on the
// offset ROW, not on the offset VALUE.
//
// A group legitimately sits at offset 0 — freshly primed on an empty log, or waiting
// for the sequencer — and keying the priming branch on `offset == 0` re-primes it on
// every tick: for a default group that is an InitOffsetLatest whose INSERT fails on
// duplicate key (logged server-side by the database, so an idle relay manufactures a
// stream of errors that look like a bug), and for a WithStartFromBeginning group it
// is a real write per tick.
func TestOffsetPrimingHappensOnceWhileTheRowExists(t *testing.T) {
	t.Run("latest group", func(t *testing.T) {
		st := newFakeStore() // empty log: the primed offset is legitimately 0
		r, err := sequence.NewRelay("c", st, noopSender)
		if err != nil {
			t.Fatalf("NewRelay: %v", err)
		}

		for range 5 {
			if err := r.RunOnce(t.Context()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
		}

		if st.snapshotInitCalls() != 1 {
			t.Fatalf("InitOffsetLatest called %d times over 5 ticks, want 1 "+
				"(a group sitting at 0 must not be re-primed every tick)", st.snapshotInitCalls())
		}
	})

	t.Run("start-from-beginning group", func(t *testing.T) {
		st := newFakeStore()
		r, err := sequence.NewRelay("c", st, noopSender, sequence.WithStartFromBeginning())
		if err != nil {
			t.Fatalf("NewRelay: %v", err)
		}

		for range 5 {
			if err := r.RunOnce(t.Context()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
		}

		if got := st.snapshotCommitCalls(); got != 1 {
			t.Fatalf("CommitOffset called %d times over 5 idle ticks, want 1 "+
				"(the seq-0 registration is a WRITE; repeating it every tick is pure churn)", got)
		}
	})
}

// TestRunTicksThenReleasesOnCancel exercises Run's fake-clock lifecycle end to
// end: the ticker drives repeated drains of a pending message, and canceling
// releases the leader lock so a planned shutdown fails over quickly. Uses
// testing/synctest's fake clock so the 1s PollInterval elapses instantly and
// deterministically.
func TestRunTicksThenReleasesOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := newFakeStore()
		st.append(msg())

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

		ctx, cancel := context.WithCancel(t.Context())
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

// --- storeWithoutSweeper ---------------------------------------------------------

// storeWithoutSweeper wraps a *fakeStore but deliberately does NOT expose
// SweepMessages (no embedding, so no method promotion), proving maybeSweep
// treats a store lacking sequence.Sweeper as retention-disabled.
type storeWithoutSweeper struct{ inner *fakeStore }

func (s storeWithoutSweeper) ListMessages(ctx context.Context, afterSeq int64, limit int) ([]*outbox.Message, error) {
	return s.inner.ListMessages(ctx, afterSeq, limit)
}

func (s storeWithoutSweeper) Offset(ctx context.Context, name string) (int64, bool, error) {
	return s.inner.Offset(ctx, name)
}

func (s storeWithoutSweeper) CommitOffset(ctx context.Context, name string, seq int64) error {
	return s.inner.CommitOffset(ctx, name, seq)
}

func (s storeWithoutSweeper) InitOffsetLatest(ctx context.Context, name string) (int64, error) {
	return s.inner.InitOffsetLatest(ctx, name)
}

func (s storeWithoutSweeper) SequenceMessages(ctx context.Context, limit int) (int, error) {
	return s.inner.SequenceMessages(ctx, limit)
}

func (s storeWithoutSweeper) TryAcquireLeaderLock(ctx context.Context, name, holderID string, ttl time.Duration) (bool, error) {
	return s.inner.TryAcquireLeaderLock(ctx, name, holderID, ttl)
}

func (s storeWithoutSweeper) ReleaseLeaderLock(ctx context.Context, name, holderID string) error {
	return s.inner.ReleaseLeaderLock(ctx, name, holderID)
}

// --- maybeSweep ---------------------------------------------------------------

// TestMaybeSweepRunsOnInterval proves the retention sweep cadence is
// wall-clock time (RetentionSweepInterval), decoupled from PollInterval: the
// first leader tick sweeps immediately, ticks within the interval skip, and a
// tick after the interval elapses sweeps again — passing the configured batch
// bound each time. (The age cutoff is not visible here at all: it is applied by
// the store, against the store's own clock.) Uses synctest's fake clock for
// deterministic elapsing.
func TestMaybeSweepRunsOnInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := newFakeStore()
		interval := 5 * time.Minute
		r, err := sequence.NewRelay("c", st, noopSender, sequence.WithRetention(interval, 100))
		if err != nil {
			t.Fatalf("NewRelay: %v", err)
		}

		// First tick sweeps immediately; two more inside the interval skip.
		for i := range 3 {
			if err := r.RunOnce(t.Context()); err != nil {
				t.Fatalf("RunOnce[%d]: %v", i, err)
			}
		}
		if calls, limit := st.snapshotSweep(); calls != 1 {
			t.Fatalf("sweepCalls = %d within the interval, want 1", calls)
		} else if limit != 100 {
			t.Fatalf("sweep batch = %d, want the configured 100", limit)
		}

		// After the interval elapses, the next tick sweeps again.
		time.Sleep(interval + time.Second)
		if err := r.RunOnce(t.Context()); err != nil {
			t.Fatalf("RunOnce[after interval]: %v", err)
		}
		if calls, _ := st.snapshotSweep(); calls != 2 {
			t.Fatalf("sweepCalls = %d after the interval elapsed, want 2", calls)
		}
	})
}

// TestNewRelayRejectsRetentionWithoutRetentionStore pins the capability
// validation: WithRetention on a store lacking sequence.Sweeper is a
// construction error, not a silently dead sweep (which would grow the log
// unboundedly and surface as a disk incident long after the misconfiguration).
func TestNewRelayRejectsRetentionWithoutRetentionStore(t *testing.T) {
	st := storeWithoutSweeper{inner: newFakeStore()}
	_, err := sequence.NewRelay("c", st, noopSender, sequence.WithRetention(time.Minute, 100))
	if err == nil || !strings.Contains(err.Error(), "Sweeper") {
		t.Fatalf("err = %v, want the Sweeper capability error", err)
	}
	// Without WithRetention the same store is fine: retention is simply off.
	if _, err := sequence.NewRelay("c", storeWithoutSweeper{inner: newFakeStore()}, noopSender); err != nil {
		t.Fatalf("NewRelay without retention: %v", err)
	}
}

// storeWithoutSequencer exposes the read/offset contract but deliberately not
// SequenceMessages, for the Sequencer capability-validation tests.
type storeWithoutSequencer struct{ inner *fakeStore }

func (s storeWithoutSequencer) ListMessages(ctx context.Context, afterSeq int64, limit int) ([]*outbox.Message, error) {
	return s.inner.ListMessages(ctx, afterSeq, limit)
}

func (s storeWithoutSequencer) Offset(ctx context.Context, name string) (int64, bool, error) {
	return s.inner.Offset(ctx, name)
}

func (s storeWithoutSequencer) CommitOffset(ctx context.Context, name string, seq int64) error {
	return s.inner.CommitOffset(ctx, name, seq)
}

func (s storeWithoutSequencer) InitOffsetLatest(ctx context.Context, name string) (int64, error) {
	return s.inner.InitOffsetLatest(ctx, name)
}

// The lock methods are delegated so this fixture lacks ONLY the Sequencer: a
// store that also could not elect would be rejected for that first, and the test
// below would pass for the wrong reason.
func (s storeWithoutSequencer) TryAcquireLeaderLock(ctx context.Context, name, holderID string, ttl time.Duration) (bool, error) {
	return s.inner.TryAcquireLeaderLock(ctx, name, holderID, ttl)
}

func (s storeWithoutSequencer) ReleaseLeaderLock(ctx context.Context, name, holderID string) error {
	return s.inner.ReleaseLeaderLock(ctx, name, holderID)
}

// TestNewRelayRejectsMissingSequencerWithoutWaiver pins the other half of the
// capability policy: a store lacking sequence.Sequencer is a
// construction error UNLESS the caller explicitly waives the sequencer with
// WithoutSequencer() (the someone-else-sequences topology). The implicit
// silent path would otherwise be a permanent, unobservable stall: nothing
// ever gets a seq, drain sees nothing, no error is reported.
func TestNewRelayRejectsMissingSequencerWithoutWaiver(t *testing.T) {
	st := storeWithoutSequencer{inner: newFakeStore()}
	_, err := sequence.NewRelay("c", st, noopSender)
	if err == nil || !strings.Contains(err.Error(), "WithoutSequencer") {
		t.Fatalf("err = %v, want the Sequencer capability error naming the waiver", err)
	}
	// The explicit waiver accepts the same store.
	if _, err := sequence.NewRelay("c", st, noopSender, sequence.WithoutSequencer()); err != nil {
		t.Fatalf("NewRelay with WithoutSequencer: %v", err)
	}
}

// TestMaybeSweepLoopsWhileFullAndReportsCounts pins the falling-behind fix:
// one sweep interval drains the whole deletable backlog in bounded full-batch
// passes (like drain), and every pass's count reaches OnSwept — a single
// silent batch per interval would let deletable rows accumulate toward a
// disk-full incident with zero warning.
func TestMaybeSweepLoopsWhileFullAndReportsCounts(t *testing.T) {
	st := newFakeStore()
	st.sweepBacklog = 250
	var swept []int
	obs := relay.Observer{OnSwept: func(_ string, n int) { swept = append(swept, n) }}
	r, err := sequence.NewRelay("c", st, noopSender,
		sequence.WithRetention(time.Minute, 100), sequence.WithObserver(obs))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if calls, _ := st.snapshotSweep(); calls != 3 {
		t.Fatalf("sweepCalls = %d, want 3 (100+100+50: loop while full, stop on the short batch)", calls)
	}
	total := 0
	for _, n := range swept {
		total += n
	}
	if total != 250 || len(swept) != 3 {
		t.Fatalf("OnSwept counts = %v (total %d), want three passes totaling 250", swept, total)
	}
}

// TestMaybeSweepErrorPropagates proves RunOnce surfaces a sweep error.
func TestMaybeSweepErrorPropagates(t *testing.T) {
	sentinel := errors.New("sweep boom")
	st := newFakeStore()
	st.sweepErr = sentinel
	r, err := sequence.NewRelay("c", st, noopSender, sequence.WithRetention(time.Minute, 100))
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
		st.append(msg())
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
		st.append(msg())
	}
	r, err := sequence.NewRelay("c", st, noopSender, sequence.WithSequenceBatchSize(2))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
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
// store implements Sequencer.
func TestWithoutSequencerSkipsSequencing(t *testing.T) {
	st := newFakeStore()
	st.append(msg())
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
	st.append(msg())
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
		st.append(msg())
	}
	if _, err := st.SequenceMessages(t.Context(), 100); err != nil {
		t.Fatalf("seed sequence: %v", err)
	}
	var sent int
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { sent++; return nil })
	r, err := sequence.NewRelay("c", st, sender,
		sequence.WithBatchSize(2), sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
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
		st.append(msg())
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

// TestOnLeadershipFiresOnTransitionsOnly pins the leadership signal: it fires
// once on becoming leader and once on losing it — never on steady-state
// renewals — so a standby takeover or a wedged-leader resume is always
// reconstructable from telemetry.
func TestOnLeadershipFiresOnTransitionsOnly(t *testing.T) {
	st := newFakeStore()
	var transitions []bool
	obs := relay.Observer{OnLeadership: func(_ string, isLeader bool) {
		transitions = append(transitions, isLeader)
	}}
	r, err := sequence.NewRelay("c", st, noopSender, sequence.WithObserver(obs))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	// Two leader ticks: exactly one became-leader transition.
	for range 2 {
		if err := r.RunOnce(t.Context()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	}
	// Demotion: another instance holds the lock; then re-promotion.
	st.mu.Lock()
	st.leader = "other"
	st.mu.Unlock()
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce[demoted]: %v", err)
	}
	st.mu.Lock()
	st.leader = ""
	st.mu.Unlock()
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce[re-promoted]: %v", err)
	}

	want := []bool{true, false, true}
	if len(transitions) != len(want) {
		t.Fatalf("transitions = %v, want %v (fire on transitions only)", transitions, want)
	}
	for i := range want {
		if transitions[i] != want[i] {
			t.Fatalf("transitions = %v, want %v", transitions, want)
		}
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
		st.append(msg())
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
		st.append(msg())
	}
	var sent int
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { sent++; return nil })

	r, err := sequence.NewRelay("c", st, sender,
		sequence.WithSequenceBatchSize(2), sequence.WithStartFromBeginning(),
		sequence.WithRetention(time.Minute, 100))
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
		st.append(msg())
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
// PoisonHandler invocation, no offset advance past the last success.
func TestCancelDuringSendDoesNotPark(t *testing.T) {
	st := newFakeStore()
	for range 5 {
		st.append(msg())
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
		sequence.WithPoisonHandler(func(_ context.Context, m *outbox.Message, _ error) error {
			parked = append(parked, m.Seq)
			return nil
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
		st.append(msg())
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
		sequence.WithPoisonHandler(func(_ context.Context, m *outbox.Message, _ error) error {
			parked = append(parked, m.Seq)
			return nil
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

// TestPoisonRowParkedWithPoisonHandler proves DecodeError routing with an
// PoisonHandler: the decoded prefix is delivered, the poison row is parked
// exactly once, the offset advances past it, and rows after it are delivered.
func TestPoisonRowParkedWithPoisonHandler(t *testing.T) {
	st := newFakeStore()
	for range 5 {
		st.append(msg())
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
		sequence.WithPoisonHandler(func(_ context.Context, m *outbox.Message, err error) error {
			parked = append(parked, m)
			parkedErrs = append(parkedErrs, err)
			return nil
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

// TestPoisonParkFailureStopsLane pins the confirmed-park contract: when the
// PoisonHandler returns an error (DLQ write failed), the offset must NOT
// advance past the poison row — committing past it is irreversible and an
// unconfirmed park would silently skip the event forever. The lane stops at
// the poison exactly as if no handler were configured, and the park retries
// next pass.
func TestPoisonParkFailureStopsLane(t *testing.T) {
	st := newFakeStore()
	for range 5 {
		st.append(msg())
	}
	st.poisonSeq = 3

	parkFailure := errors.New("dlq write failed")
	parkAttempts := 0
	r, err := sequence.NewRelay("c", st, noopSender, sequence.WithStartFromBeginning(),
		sequence.WithPoisonHandler(func(context.Context, *outbox.Message, error) error {
			parkAttempts++
			if parkAttempts == 1 {
				return parkFailure
			}
			return nil
		}))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	runErr := r.RunOnce(t.Context())
	if _, ok := errors.AsType[*sequence.DecodeError](runErr); !ok {
		t.Fatalf("RunOnce err = %v, want *DecodeError (lane stops at unconfirmed park)", runErr)
	}
	if !errors.Is(runErr, parkFailure) {
		t.Fatalf("RunOnce err = %v, want the DLQ write failure joined into the chain", runErr)
	}
	if off := st.offsets["c"]; off != 2 {
		t.Fatalf("offset = %d, want 2 (must NOT advance past the unparked poison)", off)
	}
	if parkAttempts != 1 {
		t.Fatalf("park attempts = %d, want 1", parkAttempts)
	}

	// The next pass of the SAME group retries the park; a now-healthy DLQ
	// confirms it and the lane advances past the poison.
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce[healthy dlq]: %v", err)
	}
	if off := st.offsets["c"]; off != 5 {
		t.Fatalf("offset = %d, want 5 (confirmed park advances past the poison)", off)
	}
	if parkAttempts != 2 {
		t.Fatalf("park attempts = %d, want 2 (park retried on the next pass)", parkAttempts)
	}
}

// TestPoisonRowStopsLaneWithoutPoisonHandler proves the default stop-the-lane
// behavior for a decode failure: the decoded prefix is delivered and
// committed, then the pass surfaces the *DecodeError and stops at the row.
func TestPoisonRowStopsLaneWithoutPoisonHandler(t *testing.T) {
	st := newFakeStore()
	for range 5 {
		st.append(msg())
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

// TestObserveDrainedExcludesParkedMessages pins the relay.Observer contract:
// ObserveDrained's count is successful sends only — a parked poison row is
// surfaced via ObserveError and not counted.
func TestObserveDrainedExcludesParkedMessages(t *testing.T) {
	st := newFakeStore()
	for range 5 {
		st.append(msg())
	}
	st.poisonSeq = 3
	obs := &drainCountObserver{}
	r, err := sequence.NewRelay("c", st, noopSender, sequence.WithStartFromBeginning(),
		sequence.WithObserver(relay.Observer{OnDrained: obs.ObserveDrained}),
		sequence.WithPoisonHandler(func(context.Context, *outbox.Message, error) error { return nil }))
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
		st.append(msg())
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
		st.append(msg())
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

// TestPoisonOnlyPageObservesDrained proves a page whose decoded prefix is
// empty but whose poison row is parked still reports ObserveDrained: the pass
// disposed of a message, so it must not be invisible to the lag/throughput
// signal. With no decoded row to anchor the lag on, oldestAge is zero; more
// is true (rows may follow the parked poison).
func TestPoisonOnlyPageObservesDrained(t *testing.T) {
	st := newFakeStore()
	st.append(msg())
	st.poisonSeq = 1

	obs := &fullDrainObserver{}
	r, err := sequence.NewRelay("c", st, noopSender, sequence.WithStartFromBeginning(),
		sequence.WithObserver(relay.Observer{OnDrained: obs.ObserveDrained}),
		sequence.WithPoisonHandler(func(context.Context, *outbox.Message, error) error { return nil }))
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
