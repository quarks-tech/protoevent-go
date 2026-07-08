package stream_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/stream"
)

func TestDefaultOptions(t *testing.T) {
	o := stream.DefaultOptions()
	if o.DrainWindow != time.Second {
		t.Fatalf("DrainWindow = %v, want 1s", o.DrainWindow)
	}
	if o.LeaseTTL != 15*time.Second {
		t.Fatalf("LeaseTTL = %v, want 15s", o.LeaseTTL)
	}
	if o.TokenBatchSize != 100 {
		t.Fatalf("TokenBatchSize = %d, want 100", o.TokenBatchSize)
	}
}

func TestNewRelayRejectsDrainWindowTooLarge(t *testing.T) {
	// DrainWindow must be < LeaseTTL/2 so the lease can be renewed within a window.
	_, err := stream.NewRelay("c", nil, nil,
		stream.WithLeaseTTL(10*time.Second), stream.WithDrainWindow(6*time.Second))
	if err == nil {
		t.Fatal("expected error for DrainWindow >= LeaseTTL/2, got nil")
	}
}

func TestNewRelayRejectsZeroDrainWindow(t *testing.T) {
	// A zero DrainWindow would busy-spin r.sleep (time.NewTimer(0) fires
	// immediately), so it must be rejected at construction time.
	_, err := stream.NewRelay("c", nil, nil, stream.WithDrainWindow(0))
	if err == nil {
		t.Fatal("expected error for zero DrainWindow, got nil")
	}
}

func TestNewRelayRejectsZeroTokenBatchSize(t *testing.T) {
	// A zero TokenBatchSize would make drainWindow's `for range` loop no-op
	// every call, spinning without ever draining anything.
	_, err := stream.NewRelay("c", nil, nil, stream.WithTokenBatchSize(0))
	if err == nil {
		t.Fatal("expected error for zero TokenBatchSize, got nil")
	}
}

func TestNewRelayAcceptsValidWindow(t *testing.T) {
	r, err := stream.NewRelay("c", nil, nil,
		stream.WithLeaseTTL(10*time.Second), stream.WithDrainWindow(1*time.Second))
	if err != nil || r == nil {
		t.Fatalf("NewRelay valid config: r=%v err=%v", r, err)
	}
}

// senderFunc adapts a func to eventbus.Sender.
type senderFunc func(context.Context, *event.Metadata, []byte) error

func (f senderFunc) Send(ctx context.Context, md *event.Metadata, d []byte) error {
	return f(ctx, md, d)
}

// fakeStream serves a scripted list of events, then empty windows.
type fakeStream struct {
	events     []*stream.Event
	i          int
	pbrt       string
	pbrtCT     time.Time
	closeCount int
	nextErr    error // when set, Next always returns this error instead of draining events
}

func (s *fakeStream) Next(context.Context) (*stream.Event, bool, error) {
	if s.nextErr != nil {
		return nil, false, s.nextErr
	}
	if s.i < len(s.events) {
		e := s.events[s.i]
		s.i++
		return e, true, nil
	}
	return nil, false, nil // window empty
}
func (s *fakeStream) PBRT() (string, time.Time) { return s.pbrt, s.pbrtCT }
func (s *fakeStream) Close(context.Context) error {
	s.closeCount++
	return nil
}

// fakeStreamStore hands out one fakeStream and records saved tokens.
type fakeStreamStore struct {
	mu          sync.Mutex
	stream      *fakeStream
	loadTok     string
	loadCT      time.Time
	savedTok    string
	savedCT     time.Time
	saveCount   int
	watchCount  int
	watchTokens []string // token passed to each Watch call, in order
	leader      string
}

func (s *fakeStreamStore) LoadToken(context.Context, string) (string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadTok, s.loadCT, nil
}

// SaveToken records the save and also updates loadTok/loadCT, mimicking a
// real store: the next LoadToken reflects what was just persisted.
func (s *fakeStreamStore) SaveToken(_ context.Context, _ string, tok string, ct time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.savedTok, s.savedCT, s.saveCount = tok, ct, s.saveCount+1
	s.loadTok, s.loadCT = tok, ct
	return nil
}
func (s *fakeStreamStore) Watch(_ context.Context, token string) (stream.Stream, error) {
	s.mu.Lock()
	s.watchCount++
	s.watchTokens = append(s.watchTokens, token)
	s.mu.Unlock()
	return s.stream, nil
}
func (s *fakeStreamStore) TryAcquireLeaderLock(_ context.Context, _, holderID string, _ time.Duration) (bool, error) {
	if s.leader == "" || s.leader == holderID {
		s.leader = holderID
		return true, nil
	}
	return false, nil
}
func (s *fakeStreamStore) ReleaseLeaderLock(_ context.Context, _, holderID string) error {
	if s.leader == holderID {
		s.leader = ""
	}
	return nil
}

func ev(seq int, tok string, invalidate bool) *stream.Event {
	md := event.NewMetadata("t")
	md.ID = tok
	return &stream.Event{
		Message:     &outbox.Message{ID: tok, Metadata: md, CreateTime: time.Now()},
		ResumeToken: tok,
		ClusterTime: time.Now(),
		Invalidate:  invalidate,
	}
}

func TestRunOnceDeliversAndPersistsLastToken(t *testing.T) {
	st := &fakeStreamStore{stream: &fakeStream{events: []*stream.Event{ev(1, "a", false), ev(2, "b", false)}}}
	var got []string
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		got = append(got, md.ID)
		return nil
	})
	r, _ := stream.NewRelay("c", st, sender)
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("delivered %v, want [a b]", got)
	}
	if st.savedTok != "b" {
		t.Fatalf("saved token = %q, want b (last processed)", st.savedTok)
	}
}

func TestRunOncePersistsPBRTOnEmptyWindow(t *testing.T) {
	st := &fakeStreamStore{stream: &fakeStream{events: nil, pbrt: "pbrt", pbrtCT: time.Now()}}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	r, _ := stream.NewRelay("c", st, sender)
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st.savedTok != "pbrt" {
		t.Fatalf("saved token = %q, want pbrt (empty window persists PBRT)", st.savedTok)
	}
}

func TestRunOnceStopTheLane(t *testing.T) {
	st := &fakeStreamStore{stream: &fakeStream{events: []*stream.Event{ev(1, "a", false), ev(2, "b", false), ev(3, "c", false)}}}
	var got []string
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		if md.ID == "b" {
			return errors.New("boom")
		}
		got = append(got, md.ID)
		return nil
	})
	r, _ := stream.NewRelay("c", st, sender)
	_ = r.RunOnce(t.Context())
	// Delivered a; stopped at b; c not attempted; token persisted up to a.
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("delivered %v, want [a] (stop-the-lane)", got)
	}
	if st.savedTok != "a" {
		t.Fatalf("saved token = %q, want a", st.savedTok)
	}
}

func TestRunOnceInvalidateIsFatal(t *testing.T) {
	st := &fakeStreamStore{stream: &fakeStream{events: []*stream.Event{ev(1, "x", true)}}}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	r, _ := stream.NewRelay("c", st, sender)
	err := r.RunOnce(t.Context())
	if !errors.Is(err, stream.ErrStreamInvalidated) {
		t.Fatalf("err = %v, want ErrStreamInvalidated", err)
	}
}

func TestRunOnceNonLeaderIdles(t *testing.T) {
	st := &fakeStreamStore{stream: &fakeStream{events: []*stream.Event{ev(1, "a", false)}}, leader: "other"}
	sent := 0
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { sent++; return nil })
	r, _ := stream.NewRelay("c", st, sender, stream.WithLeaderLockName("lock"))
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sent != 0 {
		t.Fatalf("non-leader sent %d, want 0", sent)
	}
}

func TestRunOnceStopClosesStreamForRedelivery(t *testing.T) {
	fs := &fakeStream{events: []*stream.Event{ev(1, "a", false), ev(2, "b", false), ev(3, "c", false)}}
	st := &fakeStreamStore{stream: fs}
	var got []string
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		if md.ID == "b" {
			return errors.New("boom")
		}
		got = append(got, md.ID)
		return nil
	})
	r, _ := stream.NewRelay("c", st, sender)
	// RunOnce returns errLaneStopped, which is unexported and unreferenceable
	// here; assert the observable effects instead.
	_ = r.RunOnce(t.Context())

	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("delivered %v, want [a] (stop before b)", got)
	}
	if fs.closeCount != 1 {
		t.Fatalf("closeCount = %d, want 1 (stream must be closed so next RunOnce reopens and redelivers b)", fs.closeCount)
	}
	if st.savedTok != "a" {
		t.Fatalf("saved token = %q, want a (last success)", st.savedTok)
	}
}

func TestRunOncePicksUpWhereItLeftOff(t *testing.T) {
	fs := &fakeStream{events: []*stream.Event{ev(1, "a", false), ev(2, "b", false), ev(3, "c", false)}}
	st := &fakeStreamStore{stream: fs}
	var got []string
	var parked []string
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		if md.ID == "b" {
			return errors.New("boom")
		}
		got = append(got, md.ID)
		return nil
	})
	r, _ := stream.NewRelay("c", st, sender, stream.WithErrorHandler(func(_ context.Context, msg *outbox.Message, _ error) {
		parked = append(parked, msg.ID)
	}))
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("delivered %v, want [a c] (b parked, not delivered)", got)
	}
	if len(parked) != 1 || parked[0] != "b" {
		t.Fatalf("parked %v, want [b]", parked)
	}
	if st.savedTok != "c" {
		t.Fatalf("saved token = %q, want c (advanced past the parked event)", st.savedTok)
	}
	if fs.closeCount != 0 {
		t.Fatalf("closeCount = %d, want 0 (park-and-continue keeps draining the same cursor)", fs.closeCount)
	}
}

// recordingObserver tracks whether ObserveError was ever called.
type recordingObserver struct {
	mu          sync.Mutex
	errObserved bool
}

func (o *recordingObserver) ObserveDrained(string, int, time.Duration, bool) {}
func (o *recordingObserver) ObserveError(string, error) {
	o.mu.Lock()
	o.errObserved = true
	o.mu.Unlock()
}

// ctxCancelingLeaderStore cancels the ctx passed to Run (via the embedded
// cancel func) from inside TryAcquireLeaderLock, then returns ctx.Err() once
// it observes the cancellation. This reproduces a leader-lock call that fails
// with context.Canceled mid-RunOnce (as opposed to Run's own top-of-loop
// ctx.Err() check, which would short-circuit before ever calling RunOnce).
type ctxCancelingLeaderStore struct {
	*fakeStreamStore
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
	st := ctxCancelingLeaderStore{fakeStreamStore: &fakeStreamStore{stream: &fakeStream{}}, cancel: cancel}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	obs := &recordingObserver{}
	r, err := stream.NewRelay("c", st, sender, stream.WithObserver(obs))
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
func (o *signalingObserver) ObserveError(string, error) {
	select {
	case o.ch <- struct{}{}:
	default:
	}
}

// TestRunObservesOpLevelDeadlineExceededWhileCtxAlive proves the shutdown-quiet
// path is gated on run-context liveness, not error identity: a genuine
// op-level context.DeadlineExceeded (e.g. the mongo v2 driver's own operation
// timeout) returned by the stream while ctx is still alive must be observed
// as a real, recurring error — not silently swallowed as a planned shutdown.
func TestRunObservesOpLevelDeadlineExceededWhileCtxAlive(t *testing.T) {
	fs := &fakeStream{nextErr: context.DeadlineExceeded}
	st := &fakeStreamStore{stream: fs}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	obs := &signalingObserver{ch: make(chan struct{}, 1)}

	r, err := stream.NewRelay("c", st, sender, stream.WithObserver(obs),
		stream.WithDrainWindow(5*time.Millisecond), stream.WithLeaseTTL(50*time.Millisecond))
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

// TestRunOnceFreshGroupPersistsBaselineBeforeDrain is the regression test for
// the at-least-once hole: a brand-new consumer group (LoadToken == "") opens
// its stream at "now", and if the FIRST drained event fails to send under
// stop-the-lane, drainWindow's processed==0 branch persists nothing. Without
// a pre-drain baseline persist, the next RunOnce would reopen with LoadToken
// still "" -> a fresh "now" -> silently skipping the failed event. RunOnce
// must persist the stream's initial resume token (PBRT, available
// immediately after Watch, before any Next) as a baseline BEFORE draining, so
// reopening after the failure resumes from a point preceding the failed
// event instead of from a fresh "now".
func TestRunOnceFreshGroupPersistsBaselineBeforeDrain(t *testing.T) {
	fs := &fakeStream{
		events: []*stream.Event{ev(1, "a", false)},
		pbrt:   "baseline",
		pbrtCT: time.Now(),
	}
	st := &fakeStreamStore{stream: fs} // loadTok == "" : fresh consumer group
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error {
		return errors.New("boom") // first event fails; no ErrorHandler -> stop-the-lane
	})
	r, _ := stream.NewRelay("c", st, sender)
	_ = r.RunOnce(t.Context())

	if len(st.watchTokens) != 1 || st.watchTokens[0] != "" {
		t.Fatalf("watch tokens = %v, want [\"\"] (fresh group opens at \"now\")", st.watchTokens)
	}
	if st.saveCount != 1 {
		t.Fatalf("saveCount = %d, want 1 (baseline persisted pre-drain; stop-the-lane with processed==0 persists nothing further)", st.saveCount)
	}
	if st.savedTok != "baseline" {
		t.Fatalf("saved token = %q, want %q (baseline persisted despite the first-event failure)", st.savedTok, "baseline")
	}

	// The next reopen must resume from the baseline, not restart at a fresh "now".
	tok, _, err := st.LoadToken(context.Background(), "c")
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if tok != "baseline" {
		t.Fatalf("LoadToken after RunOnce = %q, want %q (baseline persisted, not fresh now)", tok, "baseline")
	}
}
