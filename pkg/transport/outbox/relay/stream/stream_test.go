package stream_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/stream"
)

func TestNewRelayRejectsDrainWindowTooLarge(t *testing.T) {
	// DrainWindow must be < LeaseTTL/2 so the lease can be renewed within a
	// window. Valid store and sender, so no earlier nil-guard can mask the
	// guard under test; the message must name the violated relation.
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	_, err := stream.NewRelay("c", &fakeStore{}, sender,
		stream.WithLeaseTTL(10*time.Second), stream.WithDrainWindow(6*time.Second))
	if err == nil || !strings.Contains(err.Error(), "LeaseTTL/2") {
		t.Fatalf("err = %v, want the DrainWindow < LeaseTTL/2 validation error", err)
	}
}

func TestNewRelayRejectsOpTimeoutBelowDrainWindow(t *testing.T) {
	// DrainWindow is the change stream's server-side maxAwaitTime, so an idle
	// Next blocks for a whole window by design. An OpTimeout at or below it
	// would fire on every healthy idle window and reopen a working cursor.
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	_, err := stream.NewRelay("c", &fakeStore{}, sender,
		stream.WithDrainWindow(2*time.Second), stream.WithOpTimeout(2*time.Second))
	if err == nil || !strings.Contains(err.Error(), "OpTimeout") {
		t.Fatalf("err = %v, want the DrainWindow < OpTimeout validation error", err)
	}
}

// TestNewRelayRejectsOpTimeoutExceedingLease mirrors the sequence runtime's pin of
// the same shared invariant (relaycfg Ops.Validate): the lease must be able to
// contain the renewal cadence plus one store call, or a slow-but-successful call
// returns onto an expired lease and the relay saves a token as a stale leader.
func TestNewRelayRejectsOpTimeoutExceedingLease(t *testing.T) {
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	_, err := stream.NewRelay("c", &fakeStore{}, sender,
		stream.WithLeaseTTL(15*time.Second), stream.WithOpTimeout(30*time.Second))
	if err == nil || !strings.Contains(err.Error(), "OpTimeout") || !strings.Contains(err.Error(), "LeaseTTL") {
		t.Fatalf("err = %v, want an error relating OpTimeout to LeaseTTL", err)
	}
}

// TestFreshGroupBaselineSurvivesAFailedSave pins that a fresh consumer group does
// not silently skip events when its resume BASELINE fails to persist.
//
// A fresh group (LoadToken == "") opens at "now" and immediately persists
// Checkpoint() as its baseline, so a first-window failure that persists nothing
// still has a position preceding the failed event to reopen from. That baseline is
// a single best-effort save: when it fails — a replica-set election a millisecond
// after the aggregate succeeded — RunOnce returns, Run closes the stream and
// sleeps, and the next pass finds LoadToken == "" again and opens at a LATER
// "now". Every event committed in between is never read, never parked, never
// counted, and committedTokenAge stays 0 because committedCT is still zero, so the
// resume-token-cliff alarm reads green while events are being dropped.
//
// The baseline the relay already holds in memory is enough to close this: reopen
// from it rather than from a fresh "now".
func TestFreshGroupBaselineSurvivesAFailedSave(t *testing.T) {
	fs := &fakeStream{pbrt: "baseline-tok", pbrtCT: time.Unix(1000, 0).UTC()}
	st := &fakeStore{
		stream: fs,
		// A brand-new group: nothing stored.
		loadTok: "",
		// The baseline save fails once, then the store recovers.
		saveErr:      errors.New("not primary; election in progress"),
		saveErrTimes: 1,
	}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })

	r := mustRelay(t, st, sender,
		stream.WithDrainWindow(5*time.Millisecond), stream.WithLeaseTTL(time.Second),
		stream.WithOpTimeout(500*time.Millisecond))

	// Run, not RunOnce: it is Run that closes the stream after a failed pass and
	// therefore Run that performs the REOPEN this test is about.
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for st.watchCountSnapshot() < 2 {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("only %d Watch call(s) after the failed baseline save; wanted the reopen",
				st.watchCountSnapshot())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done

	st.mu.Lock()
	toks := slices.Clone(st.watchTokens)
	st.mu.Unlock()

	if len(toks) < 2 {
		t.Fatalf("watch tokens = %q, want at least two Watch calls", toks)
	}
	if toks[0] != "" {
		t.Fatalf("first Watch token = %q, want \"\" (a fresh group opens at now)", toks[0])
	}
	if toks[1] != "baseline-tok" {
		t.Fatalf("second Watch token = %q, want %q: reopening at a fresh \"now\" silently drops "+
			"every event committed since the first window", toks[1], "baseline-tok")
	}
}

func TestNewRelayRejectsZeroOpTimeout(t *testing.T) {
	// Zero would make every store call time out before it left the process.
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	_, err := stream.NewRelay("c", &fakeStore{}, sender, stream.WithOpTimeout(0))
	if err == nil || !strings.Contains(err.Error(), "OpTimeout must be > 0") {
		t.Fatalf("err = %v, want the OpTimeout > 0 validation error", err)
	}
}

// TestStoreCallBudgetIsOpTimeoutNotLeaseTTL pins the decoupling: the store-call
// budget is OpTimeout, not the lease. Before the split, LeaseTTL was the bound on
// every store operation, so halving the lease halved every store deadline too.
//
// The knobs stay independent, but not unrelated: the lease must be able to CONTAIN
// a pass, so DrainWindow + OpTimeout < LeaseTTL (see relaycfg Ops.Validate). This
// test used to assert that a lease FAR SHORTER than the operation budget was
// legal, which is precisely the stale-leader configuration — a 30s store call
// inside a 2s lease returns onto an expired lease and commits anyway. What it
// actually meant to pin is that within a valid pair the call still gets its full
// OpTimeout rather than the lease, so it now says that.
func TestStoreCallBudgetIsOpTimeoutNotLeaseTTL(t *testing.T) {
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })

	store := &fakeStore{stream: &fakeStream{}}

	r, err := stream.NewRelay("c", store, sender,
		// A lease much LONGER than the operation budget: the store call must get
		// its own 30s, not the 90s lease.
		stream.WithLeaseTTL(90*time.Second), stream.WithDrainWindow(500*time.Millisecond),
		stream.WithOpTimeout(30*time.Second), stream.WithoutLeaderElection())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	store.mu.Lock()
	budget := store.loadBudget
	store.mu.Unlock()

	// Generous slack: the assertion is "the 30s budget, not the 90s lease".
	if budget < 25*time.Second || budget > 60*time.Second {
		t.Fatalf("store call budget = %v, want ~30s (OpTimeout), not the 90s LeaseTTL", budget)
	}
}

func TestNewRelayRejectsZeroDrainWindow(t *testing.T) {
	// A zero DrainWindow would busy-spin r.sleep (time.NewTimer(0) fires
	// immediately), so it must be rejected at construction time.
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	_, err := stream.NewRelay("c", &fakeStore{}, sender, stream.WithDrainWindow(0))
	if err == nil || !strings.Contains(err.Error(), "DrainWindow must be > 0") {
		t.Fatalf("err = %v, want the DrainWindow > 0 validation error", err)
	}
}

func TestNewRelayRejectsZeroTokenBatchSize(t *testing.T) {
	// A zero TokenBatchSize would make drainWindow's `for range` loop no-op
	// every call, spinning without ever draining anything.
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	_, err := stream.NewRelay("c", &fakeStore{}, sender, stream.WithTokenBatchSize(0))
	if err == nil || !strings.Contains(err.Error(), "TokenBatchSize") {
		t.Fatalf("err = %v, want the TokenBatchSize validation error", err)
	}
}

func TestNewRelayRejectsEmptyName(t *testing.T) {
	// The name keys the offset/token row and is the default leader lock name;
	// an empty one would collapse every anonymous relay onto the same row.
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	_, err := stream.NewRelay("", &fakeStore{}, sender)
	if err == nil {
		t.Fatal("expected error for empty name, got nil")
	}
}

func TestNewRelayAcceptsValidWindow(t *testing.T) {
	st := &fakeStore{}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	r, err := stream.NewRelay("c", st, sender,
		stream.WithLeaseTTL(10*time.Second), stream.WithDrainWindow(1*time.Second),
		stream.WithOpTimeout(5*time.Second))
	if err != nil || r == nil {
		t.Fatalf("NewRelay valid config: r=%v err=%v", r, err)
	}
}

func TestNewRelayRejectsNilStore(t *testing.T) {
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	_, err := stream.NewRelay("c", nil, sender)
	if err == nil {
		t.Fatal("expected error for nil store, got nil")
	}
}

func TestNewRelayRejectsNilSender(t *testing.T) {
	_, err := stream.NewRelay("c", &fakeStore{}, nil)
	if err == nil {
		t.Fatal("expected error for nil sender, got nil")
	}
}

// senderFunc adapts a func to eventbus.Sender.
type senderFunc func(context.Context, *event.Metadata, []byte) error

func (f senderFunc) Send(ctx context.Context, md *event.Metadata, d []byte) error {
	return f(ctx, md, d)
}

// fakeStream serves a scripted list of events, then empty windows.
type fakeStream struct {
	events         []*stream.Event
	errs           map[int]error // when set for slot i, Next returns the error instead of events[i] (events[i] is a nil placeholder)
	i              int
	pbrt           string
	pbrtCT         time.Time
	pbrtLocalClock bool // Checkpoint reports pbrtCT as a local-clock substitute, not server-derived
	closeCount     int
	nextErr        error         // when set, Next always returns this error instead of draining events
	blockDur       time.Duration // when set, an empty-window Next sleeps this long first, mirroring the real driver's maxAwaitTime block instead of returning instantly (which would busy-spin a bubbled Run)

	// Recorded by Close, so tests can assert the close context is a fresh
	// bounded one (deadline set, not already canceled) even on shutdown paths.
	closeHadDeadline bool
	closeCtxErr      error

	closeErr error // when set, Close returns it (informational-error contract)
}

func (s *fakeStream) Next(context.Context) (*stream.Event, bool, error) {
	if s.nextErr != nil {
		return nil, false, s.nextErr
	}
	if s.i < len(s.events) {
		i := s.i
		s.i++
		if err, ok := s.errs[i]; ok {
			return nil, false, err
		}
		return s.events[i], true, nil
	}
	if s.blockDur > 0 {
		time.Sleep(s.blockDur)
	}
	return nil, false, nil // window empty
}

// Checkpoint reports its clusterTime as server-derived unless pbrtLocalClock is
// set, which models the real store's fallback for a token whose payload defies the
// known layout (mongodb.mongoStream.Checkpoint).
func (s *fakeStream) Checkpoint() (string, time.Time, bool) {
	return s.pbrt, s.pbrtCT, !s.pbrtLocalClock
}

func (s *fakeStream) Close(ctx context.Context) error {
	s.closeCount++
	_, s.closeHadDeadline = ctx.Deadline()
	s.closeCtxErr = ctx.Err()
	return s.closeErr
}

// fakeStore hands out one fakeStream and records saved tokens.
type fakeStore struct {
	mu            sync.Mutex
	stream        *fakeStream
	loadTok       string
	loadCT        time.Time
	savedTok      string
	savedCT       time.Time
	saveCount     int
	watchCount    int
	watchTokens   []string // token passed to each Watch call, in order
	watchMaxAwait time.Duration
	leader        string

	loadErr     error
	watchErr    error
	saveErr     error
	denyPersist bool // SaveToken reports (false, nil): stale save skipped by the monotone guard

	// saveErrTimes limits saveErr to the first N SaveToken calls (0 = every call),
	// modeling a transient store failure. saveErrsSoFar is its counter.
	saveErrTimes  int
	saveErrsSoFar int

	// Recorded by SaveToken, so tests can assert the final save on a shutdown
	// path goes through a fresh bounded context (deadline set, not already
	// canceled) instead of the dead run ctx.
	saveHadDeadline bool
	saveCtxErr      error

	// Recorded by LoadToken: the budget left on an ordinary bounded store call,
	// which pins that the bound is OpTimeout and not LeaseTTL.
	loadBudget time.Duration
}

func (s *fakeStore) LoadToken(ctx context.Context, _ string) (string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := ctx.Deadline(); ok {
		s.loadBudget = time.Until(d)
	}
	if s.loadErr != nil {
		return "", time.Time{}, s.loadErr
	}
	return s.loadTok, s.loadCT, nil
}

// SaveToken records the save and also updates loadTok/loadCT, mimicking a
// real store: the next LoadToken reflects what was just persisted. With
// denyPersist set it reports (false, nil) without storing anything — the
// monotone guard's "stale save skipped" path.
func (s *fakeStore) SaveToken(ctx context.Context, _, tok string, ct time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, s.saveHadDeadline = ctx.Deadline()
	s.saveCtxErr = ctx.Err()
	// saveErrTimes scopes saveErr to the first N calls, so a test can model a
	// TRANSIENT save failure (a replica-set election) rather than a store that is
	// broken forever. Zero means saveErr applies to every call, as before.
	if s.saveErr != nil && (s.saveErrTimes == 0 || s.saveErrsSoFar < s.saveErrTimes) {
		s.saveErrsSoFar++

		return false, s.saveErr
	}
	s.saveCount++
	if s.denyPersist {
		return false, nil
	}
	s.savedTok, s.savedCT = tok, ct
	s.loadTok, s.loadCT = tok, ct
	return true, nil
}

func (s *fakeStore) Watch(_ context.Context, token string, maxAwait time.Duration) (stream.Stream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watchErr != nil {
		return nil, s.watchErr
	}
	s.watchCount++
	s.watchTokens = append(s.watchTokens, token)
	s.watchMaxAwait = maxAwait
	if s.stream != nil {
		s.stream.i = 0 // each Watch opens a fresh cursor: simulate resuming/redelivering from the resume point
	}
	return s.stream, nil
}

// watchCountSnapshot reads watchCount under mu, safe to call concurrently
// with the relay's own goroutine (e.g. from a synctest observer point).
func (s *fakeStore) watchCountSnapshot() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watchCount
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

func ev(tok string) *stream.Event {
	md := event.NewMetadata("t")
	md.ID = tok
	return &stream.Event{
		Message:     &outbox.Message{ID: tok, Metadata: md, CreateTime: time.Now()},
		ResumeToken: tok,
		CommitTime:  time.Now(),
	}
}

// mustRelay builds a relay for the "c" test group, failing the test on a
// construction error instead of discarding it (a discarded error would turn
// a validation regression into a nil-receiver panic in the body).
func mustRelay(t *testing.T, st stream.Store, sender senderFunc, opts ...stream.Option) *stream.Relay {
	t.Helper()
	r, err := stream.NewRelay("c", st, sender, opts...)
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	return r
}

func TestRunOnceDeliversAndPersistsLastToken(t *testing.T) {
	st := &fakeStore{stream: &fakeStream{events: []*stream.Event{ev("a"), ev("b")}}}
	var got []string
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		got = append(got, md.ID)
		return nil
	})
	r := mustRelay(t, st, sender)
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

// evAt builds an event whose resume position carries a specific server
// clusterTime, for the lag-domain assertions.
func evAt(tok string, ct time.Time) *stream.Event {
	e := ev(tok)
	e.CommitTime = ct
	return e
}

// TestCommittedTokenAgeToleratesClockSkew pins the cliff metric's immunity to
// clock skew between the relay pod and the MongoDB primary. committedCT is a
// server clusterTime, so subtracting it from a raw host clock folds the whole
// offset into the signal: on a pod whose clock trails the primary the result goes
// NEGATIVE for a genuinely lagging token, and a gauge plots that as ~0 — so the
// one alarm meant to fire before the token falls off the oplog stays flat and the
// relay eventually hits ErrHistoryLost.
//
// The server here runs 24 hours ahead of the host, an absurd offset chosen so that
// any leak of the raw host clock into the result is unmissable. The relay first
// reaches a caught-up window (which is what calibrates the offset), then the token
// is left to age 30 minutes.
func TestCommittedTokenAgeToleratesClockSkew(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const skew = 24 * time.Hour

		serverNow := time.Now().Add(skew)

		// No events: the window ends caught up, so the Checkpoint IS the head.
		fs := &fakeStream{pbrt: "head-1", pbrtCT: serverNow}
		st := &fakeStore{stream: fs}

		var ages []time.Duration
		r := mustRelay(t, st, senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil }),
			stream.WithObserver(relay.Observer{
				OnDrained: func(_ string, _ int, oldestAge time.Duration, _ bool) {
					ages = append(ages, oldestAge)
				},
			}))

		if err := r.RunOnce(t.Context()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if len(ages) != 1 {
			t.Fatalf("OnDrained fired %d times, want 1", len(ages))
		}
		if ages[0] > time.Second {
			t.Fatalf("committedTokenAge = %v for a caught-up relay, want ~0 "+
				"(a raw host-clock measurement would report -24h or +24h)", ages[0])
		}

		// The stream head stops advancing and time passes: the committed token is
		// now genuinely 30 minutes behind the present.
		time.Sleep(30 * time.Minute)
		synctest.Wait()

		if err := r.RunOnce(t.Context()); err != nil {
			t.Fatalf("RunOnce (second): %v", err)
		}

		last := ages[len(ages)-1]
		if last < 30*time.Minute || last > 31*time.Minute {
			t.Fatalf("committedTokenAge = %v after 30m, want ~30m (skew must not leak into the signal)", last)
		}
	})
}

// TestCommittedTokenAgeCalibratesOnBusyRelay pins that the clock offset is
// calibrated on ANY caught-up window, not only on a completely event-free one.
//
// A relay under continuous load never has an event-free window: every window
// delivers something and ends in the advanced branch. If calibration lives only in
// the empty-window branch, such a relay never calibrates for its whole process
// lifetime and the lag metric silently degrades to the raw host clock — which is
// exactly the skew-blindness the offset exists to remove, and it degrades without
// any signal.
//
// The group already has a stored token, so the fresh-group baseline (which would
// calibrate on its own) is not in play; the server runs 24 hours ahead of the host
// so an uncalibrated measurement is unmistakable.
func TestCommittedTokenAgeCalibratesOnBusyRelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const skew = 24 * time.Hour

		head := time.Now().Add(skew)           // server "now"
		eventCT := head.Add(-10 * time.Minute) // events genuinely 10m behind the head

		fs := &fakeStream{
			events: []*stream.Event{evAt("a", eventCT), evAt("b", eventCT)},
			pbrt:   "head-1",
			pbrtCT: head,
		}
		// Non-empty stored token: an existing group, so RunOnce skips the
		// fresh-group baseline persist and its calibration.
		st := &fakeStore{stream: fs, loadTok: "prev", loadCT: eventCT}

		var ages []time.Duration
		r := mustRelay(t, st, senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil }),
			stream.WithObserver(relay.Observer{
				OnDrained: func(_ string, _ int, oldestAge time.Duration, _ bool) {
					ages = append(ages, oldestAge)
				},
			}))

		// One window: delivers both events AND runs out of them (caught up).
		if err := r.RunOnce(t.Context()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}

		if len(ages) != 1 {
			t.Fatalf("OnDrained fired %d times, want 1", len(ages))
		}
		if ages[0] < 9*time.Minute || ages[0] > 11*time.Minute {
			t.Fatalf("committedTokenAge = %v, want ~10m: a window that delivered events but ended caught up "+
				"must still calibrate the clock offset, or a busy relay never calibrates and the metric "+
				"falls back to the raw host clock (which reports ~0 here)", ages[0])
		}
	})
}

// TestCommittedTokenAgeGrowsWhileBacklogged pins the metric against the trap that
// anchoring it to the stream's own readings creates. A relay replaying a backlog
// reads OLD events, so the newest clusterTime it has seen equals the position it
// just committed — anchor the age to that and the gauge reads ~0 for a relay hours
// behind, silent in exactly the case the alarm exists for. (Nor can the true head
// be observed mid-backlog: ResumeToken reports the last returned document while a
// batch is in flight.)
func TestCommittedTokenAgeGrowsWhileBacklogged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Calibrate on a caught-up window, then feed a 6-hour-old backlog.
		serverNow := time.Now()
		fs := &fakeStream{pbrt: "head-1", pbrtCT: serverNow}
		st := &fakeStore{stream: fs}

		var ages []time.Duration
		r := mustRelay(t, st, senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil }),
			stream.WithObserver(relay.Observer{
				OnDrained: func(_ string, _ int, oldestAge time.Duration, _ bool) {
					ages = append(ages, oldestAge)
				},
			}))
		if err := r.RunOnce(t.Context()); err != nil {
			t.Fatalf("RunOnce (calibrate): %v", err)
		}

		// Six hours of real time pass while the relay works through events whose
		// clusterTimes are all six hours old — the backlog case.
		time.Sleep(6 * time.Hour)
		synctest.Wait()

		old := serverNow
		fs.events = []*stream.Event{evAt("old-1", old), evAt("old-2", old)}
		fs.i = 0

		if err := r.RunOnce(t.Context()); err != nil {
			t.Fatalf("RunOnce (backlog): %v", err)
		}

		last := ages[len(ages)-1]
		if last < 6*time.Hour {
			t.Fatalf("committedTokenAge = %v for a relay 6h behind, want >= 6h "+
				"(anchoring the age to the events the relay is replaying reports ~0 and never alerts)", last)
		}
	})
}

// TestCommittedTokenAgeGrowsDuringStopTheLane pins the metric against the failure
// it exists for. During an outage the relay reopens onto the same failed event
// every window, so NEITHER clusterTime advances: a pure server-domain subtraction
// freezes at ~0 for the whole incident, the cliff alert never fires, and the relay
// eventually dies with ErrHistoryLost. The reported age must keep growing with
// elapsed time even while the stream head stands still.
//
// synctest's fake clock makes the elapsed term deterministic.
func TestCommittedTokenAgeGrowsDuringStopTheLane(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		serverNow := time.Now().Add(-24 * time.Hour)

		st := &fakeStore{stream: &fakeStream{
			events:  []*stream.Event{evAt("a", serverNow), evAt("stuck", serverNow)},
			pbrt:    "a",
			pbrtCT:  serverNow,
			nextErr: nil,
		}}

		sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
			if md.ID == "stuck" {
				return errors.New("broker down")
			}
			return nil
		})

		var ages []time.Duration
		r := mustRelay(t, st, sender, stream.WithObserver(relay.Observer{
			OnDrained: func(_ string, _ int, oldestAge time.Duration, _ bool) {
				ages = append(ages, oldestAge)
			},
		}))

		// First pass: "a" is delivered and committed, "stuck" fails.
		if err := r.RunOnce(t.Context()); err != nil && !errors.Is(err, stream.ErrLaneStopped) {
			t.Fatalf("RunOnce: %v", err)
		}

		// The stream is exhausted, so subsequent windows observe no new head at
		// all — the frozen-metric scenario.
		time.Sleep(30 * time.Minute)
		synctest.Wait()

		if err := r.RunOnce(t.Context()); err != nil && !errors.Is(err, stream.ErrLaneStopped) {
			t.Fatalf("RunOnce (second): %v", err)
		}

		if len(ages) < 2 {
			t.Fatalf("OnDrained fired %d times, want >= 2", len(ages))
		}
		last := ages[len(ages)-1]
		if last < 30*time.Minute {
			t.Fatalf("committedTokenAge = %v after 30m of a stop-the-lane outage, want >= 30m "+
				"(a frozen gauge means the resume-token-cliff alert never fires)", last)
		}
	})
}

// TestCommittedTokenAgeNeverNegative pins the floor: a committed position that
// appears to LEAD the present must not produce a negative lag, which no gauge can
// represent.
//
// The commit time is therefore in the FUTURE relative to this host, which is what
// an uncalibrated relay whose clock trails the primary actually observes. A past
// commit time — which is what this test used to use — can never go negative, so
// the floor could be deleted with the test still passing.
//
// Uncalibrated is load-bearing too: the fake stream's Checkpoint returns a zero
// head, which calibrateClock refuses, so serverNow() degrades to the raw host
// clock and the subtraction really does come out below zero.
func TestCommittedTokenAgeNeverNegative(t *testing.T) {
	serverNow := time.Now().Add(24 * time.Hour)
	st := &fakeStore{stream: &fakeStream{events: []*stream.Event{evAt("a", serverNow)}}}

	var ages []time.Duration
	r := mustRelay(t, st, senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil }),
		stream.WithObserver(relay.Observer{
			OnDrained: func(_ string, _ int, oldestAge time.Duration, _ bool) {
				ages = append(ages, oldestAge)
			},
		}))
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(ages) != 1 {
		t.Fatalf("OnDrained fired %d times, want 1", len(ages))
	}
	if ages[0] < 0 {
		t.Fatalf("committedTokenAge = %v, want >= 0", ages[0])
	}
}

// TestUnsendableClassifierParksAndAdvances pins the stream runtime's escape
// hatch for an event the broker will never accept. Without it such an event is
// redelivered from the same reopened position on every window forever, and every
// event behind it stops being delivered — recoverable only by hand-editing the
// persisted token.
func TestUnsendableClassifierParksAndAdvances(t *testing.T) {
	st := &fakeStore{stream: &fakeStream{events: []*stream.Event{ev("a"), ev("bad"), ev("c")}}}

	unsendable := errors.New("body exceeds frame_max")
	var delivered []string
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		if md.ID == "bad" {
			return unsendable
		}
		delivered = append(delivered, md.ID)
		return nil
	})

	var parked []string
	r := mustRelay(t, st, sender,
		stream.WithPoisonHandler(func(_ context.Context, m *outbox.Message, _ error) error {
			parked = append(parked, m.ID)
			return nil
		}),
		stream.WithUnsendableClassifier(func(err error) bool { return errors.Is(err, unsendable) }),
	)
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(parked) != 1 || parked[0] != "bad" {
		t.Fatalf("parked = %v, want [bad]", parked)
	}
	if len(delivered) != 2 || delivered[0] != "a" || delivered[1] != "c" {
		t.Fatalf("delivered = %v, want [a c] (the lane must advance past the unsendable event)", delivered)
	}
	if st.savedTok != "c" {
		t.Fatalf("saved token = %q, want c (resume position past the parked event)", st.savedTok)
	}
}

// TestNewRelayRejectsUnsendableClassifierWithoutPoisonHandler pins the
// construction guard: without a PoisonHandler there is nowhere to park.
func TestNewRelayRejectsUnsendableClassifierWithoutPoisonHandler(t *testing.T) {
	st := &fakeStore{stream: &fakeStream{}}
	_, err := stream.NewRelay("c", st, senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil }),
		stream.WithUnsendableClassifier(func(error) bool { return true }))
	if err == nil || !strings.Contains(err.Error(), "WithPoisonHandler") {
		t.Fatalf("err = %v, want an error naming WithPoisonHandler", err)
	}
}

// TestWatchReceivesDrainWindowAsMaxAwait proves DrainWindow is the single
// latency knob: the relay threads it through Store.Watch as maxAwait.
func TestWatchReceivesDrainWindowAsMaxAwait(t *testing.T) {
	st := &fakeStore{stream: &fakeStream{}}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	r, err := stream.NewRelay("c", st, sender,
		stream.WithDrainWindow(3*time.Second), stream.WithLeaseTTL(15*time.Second),
		stream.WithOpTimeout(10*time.Second))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st.watchMaxAwait != 3*time.Second {
		t.Fatalf("Watch maxAwait = %v, want 3s (the configured DrainWindow)", st.watchMaxAwait)
	}
}

func TestRunOncePersistsPBRTOnEmptyWindow(t *testing.T) {
	st := &fakeStore{stream: &fakeStream{events: nil, pbrt: "pbrt", pbrtCT: time.Now()}}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	r := mustRelay(t, st, sender)
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st.savedTok != "pbrt" {
		t.Fatalf("saved token = %q, want pbrt (empty window persists Checkpoint)", st.savedTok)
	}
}

// TestIdleWindowsSkipIdenticalTokenSaves proves an idle relay does not
// re-persist an unchanged Checkpoint every window (~86k no-op writes/day/group on a
// quiet stream): consecutive empty windows with the same token save exactly
// once, and a changed token saves again.
func TestIdleWindowsSkipIdenticalTokenSaves(t *testing.T) {
	fs := &fakeStream{pbrt: "p1", pbrtCT: time.Now()}
	st := &fakeStore{stream: fs, loadTok: "existing"} // existing group: no fresh-group baseline save
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	r := mustRelay(t, st, sender)

	for range 3 {
		if err := r.RunOnce(t.Context()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	}
	if st.saveCount != 1 {
		t.Fatalf("saveCount = %d after 3 idle windows with unchanged Checkpoint, want 1", st.saveCount)
	}
	if st.savedTok != "p1" {
		t.Fatalf("saved token = %q, want p1", st.savedTok)
	}

	fs.pbrt, fs.pbrtCT = "p2", time.Now()
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st.saveCount != 2 || st.savedTok != "p2" {
		t.Fatalf("saveCount = %d, savedTok = %q after Checkpoint advanced, want 2, p2", st.saveCount, st.savedTok)
	}
}

func TestRunOnceStopTheLane(t *testing.T) {
	st := &fakeStore{stream: &fakeStream{events: []*stream.Event{ev("a"), ev("b"), ev("c")}}}
	var got []string
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		if md.ID == "b" {
			return errors.New("boom")
		}
		got = append(got, md.ID)
		return nil
	})
	r := mustRelay(t, st, sender)
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
	st := &fakeStore{stream: &fakeStream{nextErr: stream.ErrInvalidated}}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	r := mustRelay(t, st, sender)
	err := r.RunOnce(t.Context())
	if !errors.Is(err, stream.ErrInvalidated) {
		t.Fatalf("err = %v, want ErrInvalidated", err)
	}
}

func TestRunOnceNonLeaderIdles(t *testing.T) {
	st := &fakeStore{stream: &fakeStream{events: []*stream.Event{ev("a")}}, leader: "other"}
	sent := 0
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { sent++; return nil })
	// Small DrainWindow/LeaseTTL: RunOnce's non-leader branch now backs off one
	// DrainWindow (F3, avoids busy-spinning TryAcquireLeaderLock) before
	// returning, so keep this test fast (DrainWindow < LeaseTTL/2 guard still
	// satisfied: 10ms < 500ms).
	r := mustRelay(t, st, sender, stream.WithLeaderLockName("lock"),
		stream.WithDrainWindow(10*time.Millisecond), stream.WithLeaseTTL(1*time.Second),
		stream.WithOpTimeout(500*time.Millisecond))
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sent != 0 {
		t.Fatalf("non-leader sent %d, want 0", sent)
	}
}

func TestRunOnceStopClosesStreamForRedelivery(t *testing.T) {
	fs := &fakeStream{events: []*stream.Event{ev("a"), ev("b"), ev("c")}}
	st := &fakeStore{stream: fs}
	var got []string
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		if md.ID == "b" {
			return errors.New("boom")
		}
		got = append(got, md.ID)
		return nil
	})
	r := mustRelay(t, st, sender)
	if err := r.RunOnce(t.Context()); !errors.Is(err, stream.ErrLaneStopped) {
		t.Fatalf("RunOnce err = %v, want ErrLaneStopped (the documented back-off-and-retry signal)", err)
	}

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

// TestSendFailureStopsLaneEvenWithPoisonHandler pins the PoisonHandler's scope:
// it parks POISON events only, never send failures. A send failure is
// downstream trouble (broker down), and parking healthy events during an
// outage would bulk-divert the backlog to the DLQ while advancing the
// persisted position past it — trading away the delivery guarantee that is
// the outbox's whole point. The lane must stop at the failed event, persist
// only the sent prefix, and redeliver the failure on reopen.
func TestSendFailureStopsLaneEvenWithPoisonHandler(t *testing.T) {
	fs := &fakeStream{events: []*stream.Event{ev("a"), ev("b"), ev("c")}}
	st := &fakeStore{stream: fs}
	var got []string
	var parked []string
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		if md.ID == "b" {
			return errors.New("boom")
		}
		got = append(got, md.ID)
		return nil
	})
	r := mustRelay(t, st, sender, stream.WithPoisonHandler(func(_ context.Context, msg *outbox.Message, _ error) error {
		parked = append(parked, msg.ID)
		return nil
	}))
	_ = r.RunOnce(t.Context())

	if len(parked) != 0 {
		t.Fatalf("parked %v, want none (send failures must never be parked)", parked)
	}
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("delivered %v, want [a] (lane stops at the send failure)", got)
	}
	if st.savedTok != "a" {
		t.Fatalf("saved token = %q, want a (not advanced past the failure)", st.savedTok)
	}
	if fs.closeCount != 1 {
		t.Fatalf("closeCount = %d, want 1 (stream closed so the reopen redelivers b)", fs.closeCount)
	}
}

// countingObserver records ObserveDrained counts and whether ObserveError was
// ever called.
type countingObserver struct {
	mu          sync.Mutex
	drained     []int
	errObserved bool
}

func (o *countingObserver) ObserveDrained(_ string, count int, _ time.Duration, _ bool) {
	o.mu.Lock()
	o.drained = append(o.drained, count)
	o.mu.Unlock()
}

func (o *countingObserver) ObserveError(string, error) {
	o.mu.Lock()
	o.errObserved = true
	o.mu.Unlock()
}

// TestObserveDrainedCountsOnlySent proves the ObserveDrained count excludes
// parked messages: a parked poison event is surfaced via ObserveError instead.
func TestObserveDrainedCountsOnlySent(t *testing.T) {
	de := &stream.DecodeError{ID: "p", ResumeToken: "ptok", CommitTime: time.Now(), Err: errors.New("bad bson")}
	fs := &fakeStream{
		events: []*stream.Event{ev("a"), nil, ev("c")},
		errs:   map[int]error{1: de},
	}
	st := &fakeStore{stream: fs}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	obs := &countingObserver{}
	r := mustRelay(t, st, sender,
		stream.WithObserver(relay.Observer{OnDrained: obs.ObserveDrained, OnError: obs.ObserveError}),
		stream.WithPoisonHandler(func(context.Context, *outbox.Message, error) error { return nil }))
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.drained) != 1 || obs.drained[0] != 2 {
		t.Fatalf("ObserveDrained counts = %v, want [2] (a and c sent; parked poison not counted)", obs.drained)
	}
	if !obs.errObserved {
		t.Fatal("ObserveError not called for the parked poison event")
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
// with context.Canceled mid-RunOnce (as opposed to Run's own top-of-loop
// ctx.Err() check, which would short-circuit before ever calling RunOnce).
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
	st := ctxCancelingLeaderStore{fakeStore: &fakeStore{stream: &fakeStream{}}, cancel: cancel}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	obs := &recordingObserver{}
	r, err := stream.NewRelay("c", st, sender, stream.WithObserver(relay.Observer{OnError: obs.ObserveError}))
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

// TestShutdownDoesNotParkBufferedEvents is the regression test for the
// deploy-time DLQ flood: the mongo driver serves client-buffered events
// without touching ctx, so after a cancel the drain loop would keep pulling
// them, fail each Send with context.Canceled, and park healthy messages (up
// to TokenBatchSize spurious DLQ entries per deploy, all redelivered anyway).
// A canceled run ctx must instead stop the lane: no PoisonHandler calls, token
// not advanced past the last success.
func TestShutdownDoesNotParkBufferedEvents(t *testing.T) {
	fs := &fakeStream{events: []*stream.Event{ev("a"), ev("b"), ev("c")}}
	st := &fakeStore{stream: fs}
	ctx, cancel := context.WithCancel(t.Context())

	var got []string
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		if md.ID == "b" {
			cancel() // shutdown arrives mid-window; b and c are still buffered
			return context.Canceled
		}
		got = append(got, md.ID)
		return nil
	})
	var parked []string
	r := mustRelay(t, st, sender, stream.WithPoisonHandler(func(_ context.Context, msg *outbox.Message, _ error) error {
		parked = append(parked, msg.ID)
		return nil
	}))
	_ = r.RunOnce(ctx)

	if len(parked) != 0 {
		t.Fatalf("parked %v on shutdown, want none (healthy messages must not be DLQ'd)", parked)
	}
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("delivered %v, want [a]", got)
	}
	if st.savedTok != "a" {
		t.Fatalf("saved token = %q, want a (not advanced past the last success)", st.savedTok)
	}
	if fs.closeCount != 1 {
		t.Fatalf("closeCount = %d, want 1 (lane stopped on shutdown)", fs.closeCount)
	}
}

// TestCancelBetweenEventsStopsLane proves drainWindow's top-of-loop ctx
// check: a ctx canceled after a successful send stops the lane before the
// next client-buffered event is even pulled — no park, no delivery of the
// buffered remainder — and the final token save for the acknowledged prefix
// goes through a fresh bounded context (the run ctx is already dead), so a
// real store would not fail it and redeliver the prefix on restart.
func TestCancelBetweenEventsStopsLane(t *testing.T) {
	fs := &fakeStream{events: []*stream.Event{ev("a"), ev("b"), ev("c")}}
	st := &fakeStore{stream: fs, loadTok: "existing"} // existing group: no baseline save
	ctx, cancel := context.WithCancel(t.Context())

	var got []string
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		got = append(got, md.ID)
		if md.ID == "a" {
			cancel() // shutdown lands after this send succeeds; b and c stay buffered
		}
		return nil
	})
	var parked []string
	r := mustRelay(t, st, sender, stream.WithPoisonHandler(func(_ context.Context, msg *outbox.Message, _ error) error {
		parked = append(parked, msg.ID)
		return nil
	}))
	_ = r.RunOnce(ctx)

	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("delivered %v, want [a] (buffered events must not be pulled after cancel)", got)
	}
	if len(parked) != 0 {
		t.Fatalf("parked %v on shutdown, want none", parked)
	}
	if st.savedTok != "a" {
		t.Fatalf("saved token = %q, want a (the acknowledged prefix)", st.savedTok)
	}
	if fs.closeCount != 1 {
		t.Fatalf("closeCount = %d, want 1 (lane stopped on shutdown)", fs.closeCount)
	}
	if st.saveCtxErr != nil {
		t.Fatalf("final save ctx already dead (%v), want a live fresh context", st.saveCtxErr)
	}
	if !st.saveHadDeadline {
		t.Fatal("final save ctx had no deadline, want a bounded fresh context")
	}
}

// TestDecodeErrorParksAndContinues proves the poison-event path: with an
// PoisonHandler, a *DecodeError from Next is parked exactly once, the window
// keeps draining, subsequent events are delivered, and the token advances
// past the poison event.
func TestDecodeErrorParksAndContinues(t *testing.T) {
	de := &stream.DecodeError{ID: "p", ResumeToken: "ptok", CommitTime: time.Now(), Err: errors.New("bad bson")}
	fs := &fakeStream{
		events: []*stream.Event{ev("a"), nil, ev("c")},
		errs:   map[int]error{1: de},
	}
	st := &fakeStore{stream: fs}
	var got []string
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		got = append(got, md.ID)
		return nil
	})
	var parked []string
	r := mustRelay(t, st, sender, stream.WithPoisonHandler(func(_ context.Context, msg *outbox.Message, err error) error {
		if _, ok := errors.AsType[*stream.DecodeError](err); !ok {
			t.Errorf("PoisonHandler err = %v, want a *DecodeError", err)
		}
		parked = append(parked, msg.ID)
		return nil
	}))
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(parked) != 1 || parked[0] != "p" {
		t.Fatalf("parked %v, want [p]", parked)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("delivered %v, want [a c] (window continues past the poison event)", got)
	}
	if st.savedTok != "c" {
		t.Fatalf("saved token = %q, want c (advanced past the poison event)", st.savedTok)
	}
	if fs.closeCount != 0 {
		t.Fatalf("closeCount = %d, want 0 (lane keeps moving)", fs.closeCount)
	}
}

// TestZeroClusterTimePoisonDoesNotRewindWindowCT is the regression test for
// the lag-metric rewind: a poison event whose CommitTime extraction failed
// (zero) must not drag the window's running commitTime below what an earlier
// event in the SAME window already advanced it to — the final save would
// otherwise persist a stale anchor (the $lte-equal path accepts it) and
// inflate committedTokenAge during poison episodes.
func TestZeroClusterTimePoisonDoesNotRewindWindowCT(t *testing.T) {
	evCT := time.Now().UTC().Truncate(time.Second)
	a := ev("a")
	a.CommitTime = evCT
	de := &stream.DecodeError{ID: "p", ResumeToken: "ptok", Err: errors.New("bad bson")} // CommitTime zero
	fs := &fakeStream{
		events: []*stream.Event{a, nil},
		errs:   map[int]error{1: de},
	}
	st := &fakeStore{stream: fs, loadTok: "existing"}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	r := mustRelay(t, st, sender, stream.WithPoisonHandler(func(context.Context, *outbox.Message, error) error { return nil }))
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if st.savedTok != "ptok" {
		t.Fatalf("saved token = %q, want ptok (advanced past the parked poison)", st.savedTok)
	}
	if !st.savedCT.Equal(evCT) {
		t.Fatalf("saved commitTime = %v, want %v (the in-window event's CT — a zero-CT poison must floor, not rewind)", st.savedCT, evCT)
	}
}

// TestPoisonParkFailureStopsLane pins the confirmed-park contract: when the
// PoisonHandler returns an error, the token must NOT advance past the poison
// event — the lane stops exactly as if no handler were configured (prefix
// persisted, DecodeError surfaced), and the park retries on reopen.
func TestPoisonParkFailureStopsLane(t *testing.T) {
	de := &stream.DecodeError{ID: "p", ResumeToken: "ptok", CommitTime: time.Now(), Err: errors.New("bad bson")}
	fs := &fakeStream{
		events: []*stream.Event{ev("a"), nil},
		errs:   map[int]error{1: de},
	}
	st := &fakeStore{stream: fs, loadTok: "existing"}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	parkFailure := errors.New("dlq write failed")
	parkAttempts := 0
	r := mustRelay(t, st, sender,
		stream.WithDrainWindow(5*time.Millisecond), stream.WithLeaseTTL(time.Second),
		stream.WithOpTimeout(500*time.Millisecond),
		stream.WithPoisonHandler(func(context.Context, *outbox.Message, error) error {
			parkAttempts++
			if parkAttempts == 1 {
				return parkFailure
			}
			return nil
		}))

	runErr := r.RunOnce(t.Context())
	if _, ok := errors.AsType[*stream.DecodeError](runErr); !ok {
		t.Fatalf("RunOnce err = %v, want *DecodeError (lane stops at unconfirmed park)", runErr)
	}
	if !errors.Is(runErr, parkFailure) {
		t.Fatalf("RunOnce err = %v, want the DLQ write failure joined into the chain", runErr)
	}
	if parkAttempts != 1 {
		t.Fatalf("park attempts = %d, want 1", parkAttempts)
	}
	if st.savedTok != "a" {
		t.Fatalf("saved token = %q, want a (prefix persisted; NOT advanced past the unparked poison)", st.savedTok)
	}

	// Run reacts to the pass error by closing the cursor and reopening from
	// the persisted position (its default transient branch); RunOnce leaves
	// that to the caller. Simulate the close via a demotion pass, then a
	// re-acquiring pass reopens at "a" and retries the park — a now-healthy
	// DLQ confirms it and the lane advances past the poison.
	st.mu.Lock()
	st.leader = "other"
	st.mu.Unlock()
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce[demoted]: %v", err)
	}
	if fs.closeCount != 1 {
		t.Fatalf("closeCount = %d, want 1 (cursor released so the retry reopens from the persisted token)", fs.closeCount)
	}
	st.mu.Lock()
	st.leader = ""
	st.mu.Unlock()

	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce[healthy dlq]: %v", err)
	}
	if got := st.watchTokens; len(got) != 2 || got[1] != "a" {
		t.Fatalf("watch tokens = %v, want a second Watch resuming from the persisted prefix token a", got)
	}
	if parkAttempts != 2 {
		t.Fatalf("park attempts = %d, want 2 (park retried on reopen)", parkAttempts)
	}
	if st.savedTok != "ptok" {
		t.Fatalf("saved token = %q, want ptok (confirmed park advances past the poison)", st.savedTok)
	}
}

// TestDecodeErrorWithoutHandlerStopsLane proves that without an PoisonHandler
// a *DecodeError stops the lane: the sent prefix's token IS persisted (so the
// reopen resumes at the poison instead of re-sending the prefix every
// DrainWindow), but the token is NOT advanced past the poison event itself,
// preserving order (at-least-once redelivery on reopen).
func TestDecodeErrorWithoutHandlerStopsLane(t *testing.T) {
	de := &stream.DecodeError{ID: "p", ResumeToken: "ptok", CommitTime: time.Now(), Err: errors.New("bad bson")}
	fs := &fakeStream{
		events: []*stream.Event{ev("a"), nil},
		errs:   map[int]error{1: de},
	}
	st := &fakeStore{stream: fs, loadTok: "existing"} // existing group: no baseline save
	var got []string
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		got = append(got, md.ID)
		return nil
	})
	r := mustRelay(t, st, sender)
	runErr := r.RunOnce(t.Context())

	if _, ok := errors.AsType[*stream.DecodeError](runErr); !ok {
		t.Fatalf("RunOnce err = %v, want a *DecodeError", runErr)
	}
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("delivered %v, want [a]", got)
	}
	if st.saveCount != 1 || st.savedTok != "a" {
		t.Fatalf("saveCount = %d, savedTok = %q, want the pre-poison success token %q saved exactly once (prefix persisted; not advanced past the poison)", st.saveCount, st.savedTok, "a")
	}
}

// TestDecodeErrorWithEmptyTokenIsNotParkable proves an empty ResumeToken makes
// a poison event non-parkable even with an PoisonHandler configured: the
// store's token extraction is best-effort, and parking would later persist ""
// and erase the group's position (restart "at now", silently skipping
// events). The lane stops instead — the prefix's token is saved, the poison
// is not advanced past, and no save ever carries "".
func TestDecodeErrorWithEmptyTokenIsNotParkable(t *testing.T) {
	de := &stream.DecodeError{ID: "p", ResumeToken: "", CommitTime: time.Now(), Err: errors.New("bad bson")}
	fs := &fakeStream{
		events: []*stream.Event{ev("a"), nil},
		errs:   map[int]error{1: de},
	}
	st := &fakeStore{stream: fs, loadTok: "existing"} // existing group: no baseline save
	var got []string
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		got = append(got, md.ID)
		return nil
	})
	var parked []string
	r := mustRelay(t, st, sender, stream.WithPoisonHandler(func(_ context.Context, msg *outbox.Message, _ error) error {
		parked = append(parked, msg.ID)
		return nil
	}))
	runErr := r.RunOnce(t.Context())

	if _, ok := errors.AsType[*stream.DecodeError](runErr); !ok {
		t.Fatalf("RunOnce err = %v, want a *DecodeError (lane must stop)", runErr)
	}
	if len(parked) != 0 {
		t.Fatalf("parked %v, want none (an empty-token poison event is non-parkable)", parked)
	}
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("delivered %v, want [a]", got)
	}
	if st.saveCount != 1 || st.savedTok != "a" {
		t.Fatalf("saveCount = %d, savedTok = %q, want exactly one save of %q (never the empty token)", st.saveCount, st.savedTok, "a")
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
// op-level context.DeadlineExceeded (e.g. the mongo v2 driver's own operation
// timeout) returned by the stream while ctx is still alive must be observed
// as a real, recurring error — not silently swallowed as a planned shutdown.
func TestRunObservesOpLevelDeadlineExceededWhileCtxAlive(t *testing.T) {
	fs := &fakeStream{nextErr: context.DeadlineExceeded}
	st := &fakeStore{stream: fs}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	obs := &signalingObserver{ch: make(chan struct{}, 1)}

	r, err := stream.NewRelay("c", st, sender, stream.WithObserver(relay.Observer{OnError: obs.ObserveError}),
		stream.WithDrainWindow(5*time.Millisecond), stream.WithLeaseTTL(50*time.Millisecond),
		stream.WithOpTimeout(40*time.Millisecond))
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
// stop-the-lane, drainWindow's non-advanced branch persists nothing. Without
// a pre-drain baseline persist, the next RunOnce would reopen with LoadToken
// still "" -> a fresh "now" -> silently skipping the failed event. RunOnce
// must persist the stream's initial resume token (Checkpoint, available
// immediately after Watch, before any Next) as a baseline BEFORE draining, so
// reopening after the failure resumes from a point preceding the failed
// event instead of from a fresh "now".
func TestRunOnceFreshGroupPersistsBaselineBeforeDrain(t *testing.T) {
	fs := &fakeStream{
		events: []*stream.Event{ev("a")},
		pbrt:   "baseline",
		pbrtCT: time.Now(),
	}
	st := &fakeStore{stream: fs} // loadTok == "" : fresh consumer group
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error {
		return errors.New("boom") // first event fails; no PoisonHandler -> stop-the-lane
	})
	r := mustRelay(t, st, sender)
	_ = r.RunOnce(t.Context())

	if len(st.watchTokens) != 1 || st.watchTokens[0] != "" {
		t.Fatalf("watch tokens = %v, want [\"\"] (fresh group opens at \"now\")", st.watchTokens)
	}
	if st.saveCount != 1 {
		t.Fatalf("saveCount = %d, want 1 (baseline persisted pre-drain; stop-the-lane with no successes persists nothing further)", st.saveCount)
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

// TestStopBackoffThenReopens exercises Run's fake-clock backoff lifecycle: a
// stop-the-lane send failure closes the stream and backs off exactly one
// DrainWindow (fake time) before reopening and redelivering the failed
// event. Uses testing/synctest's fake clock so DrainWindow elapses instantly
// and deterministically; the fake stream's empty-window Next blocks for
// DrainWindow (mirroring the real change stream's maxAwaitTime) so the Run
// goroutine is durably blocked between watches instead of busy-spinning the
// bubble.
func TestStopBackoffThenReopens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const window = time.Second
		fs := &fakeStream{
			events:   []*stream.Event{ev("b")},
			blockDur: window,
		}
		st := &fakeStore{stream: fs}

		var attempts, delivered atomic.Int64
		sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
			if attempts.Add(1) == 1 {
				return errors.New("boom") // first delivery attempt fails: stop-the-lane
			}
			delivered.Add(1)
			return nil
		})

		r, err := stream.NewRelay("c", st, sender, stream.WithDrainWindow(window))
		if err != nil {
			t.Fatalf("NewRelay: %v", err)
		}

		// defer cancel(), not just the cancel() at the end of the happy path: any
		// t.Fatalf before it would otherwise leave Run blocked inside the synctest
		// bubble forever, and an exiting bubble with a live goroutine panics the
		// whole package binary — so one regression here silently takes every later
		// test in the file with it.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan struct{})
		go func() {
			_ = r.Run(ctx)
			close(done)
		}()

		// Immediately after the failure: Run must have closed the stream and
		// be backing off, not already reopened.
		synctest.Wait()
		if got := st.watchCountSnapshot(); got != 1 {
			t.Fatalf("watchCount = %d immediately after the failure, want 1 (must back off before reopening)", got)
		}

		// One DrainWindow later: Run reopens and redelivers the failed event.
		time.Sleep(window)
		synctest.Wait()
		if got := st.watchCountSnapshot(); got != 2 {
			t.Fatalf("watchCount = %d after one DrainWindow, want 2 (must reopen after exactly one backoff)", got)
		}
		if delivered.Load() != 1 {
			t.Fatalf("delivered = %d, want 1 (the failed event must be redelivered on reopen)", delivered.Load())
		}

		// Cancel to exit; pump the fake clock until Run drains its pending
		// (fake-time-blocked) window wait and returns, so the bubble doesn't
		// exit with Run still alive.
		cancel()
		for range 10 {
			select {
			case <-done:
				return
			default:
			}
			time.Sleep(window)
			synctest.Wait()
		}
		t.Fatal("Run did not exit after cancel")
	})
}

// countingHandler is a slog.Handler recording the records it received.
//
// It keeps the MESSAGES, not just a count. A bare count is satisfied by any log
// line at all, and this relay emits an unconditional "became leader" Info on its
// first pass — so every "the logger was invoked" assertion passed with the exact
// call it claimed to pin deleted.
type countingHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, rec slog.Record) error {
	h.mu.Lock()
	h.msgs = append(h.msgs, rec.Message)
	h.mu.Unlock()
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

// logged reports whether any record carried exactly msg.
func (h *countingHandler) logged(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Contains(h.msgs, msg)
}

// messages returns every recorded message, for failure output.
func (h *countingHandler) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.msgs)
}

func TestNewRelayRejectsZeroLeaseTTL(t *testing.T) {
	// DrainWindow stays at its positive default (1s), so this isolates the
	// LeaseTTL <= 0 branch specifically.
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	_, err := stream.NewRelay("c", &fakeStore{}, sender, stream.WithLeaseTTL(0))
	if err == nil || !strings.Contains(err.Error(), "LeaseTTL must be > 0") {
		t.Fatalf("err = %v, want the LeaseTTL > 0 validation error", err)
	}
}

// TestLeaderDemotionClosesStreamAndReloadsToken pins the demoted-leader
// hazard: once TryAcquire reports the lease lost, the open cursor MUST be
// closed (a demoted leader consuming a live cursor would race the new
// leader), and a later re-promotion MUST reload the position from the store
// (LoadToken → Watch) rather than resume the stale in-memory cursor — the
// interim leader may have advanced the persisted token, and resuming the old
// cursor would re-send everything it delivered.
func TestLeaderDemotionClosesStreamAndReloadsToken(t *testing.T) {
	fs := &fakeStream{events: []*stream.Event{ev("a"), ev("b")}}
	st := &fakeStore{stream: fs}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	r := mustRelay(t, st, sender,
		stream.WithDrainWindow(5*time.Millisecond), stream.WithLeaseTTL(time.Second),
		stream.WithOpTimeout(500*time.Millisecond))

	// Window 1: leader, delivers a+b, persists token "b".
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce[leader]: %v", err)
	}
	if st.savedTok != "b" {
		t.Fatalf("saved token = %q, want b", st.savedTok)
	}

	// Demotion: another instance holds the lock.
	st.mu.Lock()
	st.leader = "other"
	st.mu.Unlock()
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce[demoted]: %v", err)
	}
	if fs.closeCount != 1 {
		t.Fatalf("closeCount = %d, want 1 (demotion must close the live cursor)", fs.closeCount)
	}

	// Re-promotion: the lock frees up; the relay must reopen from the STORED
	// token, not the stale cursor.
	st.mu.Lock()
	st.leader = ""
	st.mu.Unlock()
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce[re-promoted]: %v", err)
	}
	if got := st.watchTokens; len(got) != 2 || got[1] != "b" {
		t.Fatalf("watch tokens = %v, want a second Watch resuming from the stored token b", got)
	}
}

// TestCloseStreamErrorIsLoggedNotFatal pins closeStream's informational-error
// contract (mirroring LeaderElector.Release): a failed cursor close is logged
// (Warn) and the pass continues unaffected — the local cursor is dropped
// regardless, and the server reaps the leaked cursor on its own timeout.
func TestCloseStreamErrorIsLoggedNotFatal(t *testing.T) {
	fs := &fakeStream{closeErr: errors.New("killCursors boom")}
	st := &fakeStore{stream: fs, loadTok: "existing"}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	handler := &countingHandler{}
	r := mustRelay(t, st, sender, stream.WithLogger(slog.New(handler)),
		stream.WithDrainWindow(5*time.Millisecond), stream.WithLeaseTTL(time.Second),
		stream.WithOpTimeout(500*time.Millisecond))

	// Open the stream as leader, then get demoted: the demotion path closes
	// the cursor, whose Close fails.
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce[leader]: %v", err)
	}
	st.mu.Lock()
	st.leader = "other"
	st.mu.Unlock()
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce[demoted] = %v, want nil (a failed close is informational, not a pass error)", err)
	}
	if fs.closeCount != 1 {
		t.Fatalf("closeCount = %d, want 1", fs.closeCount)
	}
	if !handler.logged("stream relay: close stream") {
		t.Fatalf("failed Close was not logged; the informational error must not be silently discarded (records: %v)",
			handler.messages())
	}
	// The reopen path is unaffected: re-promotion opens a fresh cursor.
	st.mu.Lock()
	st.leader = ""
	st.mu.Unlock()
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce[re-promoted]: %v", err)
	}
	if st.watchCountSnapshot() != 2 {
		t.Fatalf("watchCount = %d, want 2 (fresh cursor after the failed close)", st.watchCountSnapshot())
	}
}

// TestUnpersistedSaveDoesNotAdvanceLocalTrackers pins the persisted=false
// contract: a save the store's monotone guard skipped must not advance the
// relay's local committed-position trackers. Observable via the idle-window
// no-op-save skip: with persistence denied, lastSavedTok never latches, so an
// unchanged Checkpoint is re-saved every window instead of being skipped after the
// first.
func TestUnpersistedSaveDoesNotAdvanceLocalTrackers(t *testing.T) {
	fs := &fakeStream{pbrt: "p1", pbrtCT: time.Now()}
	st := &fakeStore{stream: fs, loadTok: "existing", denyPersist: true}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	r := mustRelay(t, st, sender)

	for range 3 {
		if err := r.RunOnce(t.Context()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	}
	if st.saveCount != 3 {
		t.Fatalf("saveCount = %d, want 3 (an unpersisted save must not latch lastSavedTok and suppress retries)", st.saveCount)
	}
}

// TestRunReturnsInvalidateAsFatal proves the invalidate flow end to end:
// Stream.Next surfacing ErrInvalidated is fatal (Run returns it), the
// error reaches ObserveError, and the stream is closed on the way out.
func TestRunReturnsInvalidateAsFatal(t *testing.T) {
	fs := &fakeStream{nextErr: stream.ErrInvalidated}
	st := &fakeStore{stream: fs}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	obs := &recordingObserver{}
	r, err := stream.NewRelay("c", st, sender, stream.WithObserver(relay.Observer{OnError: obs.ObserveError}))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	runErr := r.Run(t.Context())
	if !errors.Is(runErr, stream.ErrInvalidated) {
		t.Fatalf("Run err = %v, want ErrInvalidated", runErr)
	}
	obs.mu.Lock()
	errObserved := obs.errObserved
	obs.mu.Unlock()
	if !errObserved {
		t.Fatal("ObserveError not called for the fatal invalidate")
	}
	if fs.closeCount != 1 {
		t.Fatalf("closeCount = %d, want 1 (fatal exit must still close the stream)", fs.closeCount)
	}
}

// TestPlannedShutdownClosesStreamWithBoundedFreshContext proves the
// cursor-leak fix: a planned shutdown closes the open stream through
// closeStream's fresh bounded context (deadline set, not already canceled),
// so the killCursors is both deliverable and bounded even though the run ctx
// is dead.
func TestPlannedShutdownClosesStreamWithBoundedFreshContext(t *testing.T) {
	fs := &fakeStream{pbrt: "p", pbrtCT: time.Now()}
	st := &fakeStore{stream: fs, loadTok: "existing"}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	r, err := stream.NewRelay("c", st, sender)
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	if err := r.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if fs.closeCount != 0 {
		t.Fatalf("closeCount = %d before shutdown, want 0", fs.closeCount)
	}

	// Planned shutdown: Run returns immediately on the dead ctx, and its
	// deferred closeStream must still deliver the close.
	cancel()
	if runErr := r.Run(ctx); !errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", runErr)
	}
	if fs.closeCount != 1 {
		t.Fatalf("closeCount = %d after shutdown, want 1 (cursor must not leak)", fs.closeCount)
	}
	if !fs.closeHadDeadline {
		t.Fatal("Close ctx had no deadline, want a bounded fresh context")
	}
	if fs.closeCtxErr != nil {
		t.Fatalf("Close ctx already dead (%v), want a live fresh context", fs.closeCtxErr)
	}
}

// TestWithLoggerReceivesTransientError proves a custom Logger set via
// WithLogger is invoked (not just the default discard logger) on a transient,
// ctx-alive error in Run's default branch.
func TestWithLoggerReceivesTransientError(t *testing.T) {
	fs := &fakeStream{nextErr: errors.New("boom")}
	st := &fakeStore{stream: fs}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	obs := &signalingObserver{ch: make(chan struct{}, 1)}
	handler := &countingHandler{}

	r, err := stream.NewRelay("c", st, sender,
		stream.WithObserver(relay.Observer{OnError: obs.ObserveError}), stream.WithLogger(slog.New(handler)),
		stream.WithDrainWindow(5*time.Millisecond), stream.WithLeaseTTL(50*time.Millisecond),
		stream.WithOpTimeout(40*time.Millisecond))
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
		t.Fatal("ObserveError was not called for the transient error")
	}

	cancel()
	if runErr := <-done; !errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", runErr)
	}
	if !handler.logged("stream relay: pass failed") {
		t.Fatalf("custom slog handler was never invoked for the transient error (records: %v)", handler.messages())
	}
}

// TestRunOnceLoadTokenErrorPropagates proves a Store.LoadToken error
// surfaces directly from RunOnce.
func TestRunOnceLoadTokenErrorPropagates(t *testing.T) {
	sentinel := errors.New("load boom")
	st := &fakeStore{stream: &fakeStream{}, loadErr: sentinel}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	r, err := stream.NewRelay("c", st, sender)
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	runErr := r.RunOnce(t.Context())
	if !errors.Is(runErr, sentinel) {
		t.Fatalf("RunOnce err = %v, want %v", runErr, sentinel)
	}
}

// TestRunOnceWatchErrorPropagates proves a Store.Watch error surfaces
// directly from RunOnce.
func TestRunOnceWatchErrorPropagates(t *testing.T) {
	sentinel := errors.New("watch boom")
	st := &fakeStore{stream: &fakeStream{}, watchErr: sentinel}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	r, err := stream.NewRelay("c", st, sender)
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	runErr := r.RunOnce(t.Context())
	if !errors.Is(runErr, sentinel) {
		t.Fatalf("RunOnce err = %v, want %v", runErr, sentinel)
	}
}

// TestRunOnceBaselineSaveTokenErrorPropagates proves a fresh consumer group
// (LoadToken == "") that fails to persist the pre-drain Checkpoint baseline
// surfaces that SaveToken error directly from RunOnce, before ever draining.
func TestRunOnceBaselineSaveTokenErrorPropagates(t *testing.T) {
	sentinel := errors.New("save boom")
	fs := &fakeStream{events: []*stream.Event{ev("a")}, pbrt: "baseline", pbrtCT: time.Now()}
	st := &fakeStore{stream: fs, saveErr: sentinel} // loadTok == "": fresh group
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	r, err := stream.NewRelay("c", st, sender)
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	runErr := r.RunOnce(t.Context())
	if !errors.Is(runErr, sentinel) {
		t.Fatalf("RunOnce err = %v, want %v", runErr, sentinel)
	}
}

// TestDrainWindowSaveTokenErrorOnDeliveryPropagates proves a SaveToken error
// on the main post-delivery persist path surfaces from drainWindow/RunOnce.
// The store's loadTok is pre-set to a non-empty value so the pre-drain
// baseline persist (a different SaveToken call site) is skipped.
func TestDrainWindowSaveTokenErrorOnDeliveryPropagates(t *testing.T) {
	sentinel := errors.New("save boom")
	fs := &fakeStream{events: []*stream.Event{ev("a")}}
	st := &fakeStore{stream: fs, loadTok: "existing", saveErr: sentinel}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	r, err := stream.NewRelay("c", st, sender)
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	runErr := r.RunOnce(t.Context())
	if !errors.Is(runErr, sentinel) {
		t.Fatalf("RunOnce err = %v, want %v", runErr, sentinel)
	}
}

// TestDrainWindowSaveTokenErrorOnEmptyWindowPropagates proves a SaveToken
// error on the empty-window Checkpoint persist path surfaces from
// drainWindow/RunOnce. loadTok is pre-set so the pre-drain baseline persist
// (which would otherwise also hit saveErr) is skipped.
func TestDrainWindowSaveTokenErrorOnEmptyWindowPropagates(t *testing.T) {
	sentinel := errors.New("save boom")
	fs := &fakeStream{events: nil, pbrt: "pbrt", pbrtCT: time.Now()}
	st := &fakeStore{stream: fs, loadTok: "existing", saveErr: sentinel}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	r, err := stream.NewRelay("c", st, sender)
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	runErr := r.RunOnce(t.Context())
	if !errors.Is(runErr, sentinel) {
		t.Fatalf("RunOnce err = %v, want %v", runErr, sentinel)
	}
}

// TestDecodeErrorWithEmptyIDIsParkable proves an empty DecodeError.ID does NOT
// make a poison event non-parkable, as long as the resume token survived.
//
// The two extractions are independent and both best-effort (watch.go pulls the
// token from the change document's _id and the row id from fullDocument._id), so
// a corrupted fullDocument yields a good token with an empty id. Refusing to park
// that used to leave the stream reopening onto the same event every window
// forever, with a PoisonHandler installed and willing to take it — nothing behind
// it ever delivered. An idless stub is a worse record than an identified one, but
// it is not a wedge, and the sequence runtime (which gates on the handler alone)
// has always made the same trade.
//
// The token is what remains load-bearing: see
// TestDecodeErrorWithEmptyTokenIsNotParkable.
func TestDecodeErrorWithEmptyIDIsParkable(t *testing.T) {
	de := &stream.DecodeError{ID: "", ResumeToken: "ptok", CommitTime: time.Now(), Err: errors.New("bad bson")}
	fs := &fakeStream{
		events: []*stream.Event{ev("a"), nil},
		errs:   map[int]error{1: de},
	}
	st := &fakeStore{stream: fs, loadTok: "existing"}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })

	parked := 0
	r := mustRelay(t, st, sender, stream.WithPoisonHandler(func(_ context.Context, _ *outbox.Message, _ error) error {
		parked++
		return nil
	}))

	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce err = %v, want nil (the poison event is parked and the lane advances)", err)
	}
	if parked != 1 {
		t.Fatalf("parked %d events, want 1", parked)
	}
	if st.savedTok != "ptok" {
		t.Fatalf("savedTok = %q, want %q (the lane must resume past the parked poison event)", st.savedTok, "ptok")
	}
}

// --- IndexEnsurer ---------------------------------------------------------------

// ensuringStore is a fakeStore that also offers the optional IndexEnsurer
// capability, so the relay's start-up call can be observed.
type ensuringStore struct {
	*fakeStore
	mu    sync.Mutex
	calls int
	err   error
}

func (s *ensuringStore) EnsureIndexes(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.err
}

func (s *ensuringStore) snapshotEnsure() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestRunEnsuresTheStoresRetentionIndexes pins that a capability implied by
// configuration actually exists.
//
// This runtime has no sweep of its own — MongoDB retention is a server-side TTL
// index — so it also has no signal when retention is simply absent. A deployment
// that built its store WithRetention but never called EnsureIndexes got no error,
// no log and no observer signal, just a collection growing to a disk incident.
// The sequence runtime solves the same problem for TiDB explicitly; this closes
// the gap here without adding a second thing for the operator to remember.
func TestRunEnsuresTheStoresRetentionIndexes(t *testing.T) {
	st := &ensuringStore{fakeStore: &fakeStore{stream: &fakeStream{}}}
	r := mustRelay(t, st, senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil }),
		stream.WithDrainWindow(time.Millisecond), stream.WithLeaseTTL(time.Second),
		stream.WithOpTimeout(500*time.Millisecond))

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)

	if got := st.snapshotEnsure(); got != 1 {
		t.Fatalf("EnsureIndexes called %d times, want exactly 1 at start "+
			"(a store configured with a retention whose index is never created grows without bound)", got)
	}
}

// TestRunSurvivesAFailedEnsureIndexes pins that the call above is advisory.
//
// The two ways it fails are both survivable: relay credentials that cannot issue
// DDL because indexes are managed by migrations, and a retention value changed
// against an existing collection (which MongoDB rejects outright, requiring
// collMod). Failing Run on either would turn a retention misconfiguration into a
// total delivery outage — strictly worse than the unbounded growth it warns about.
func TestRunSurvivesAFailedEnsureIndexes(t *testing.T) {
	st := &ensuringStore{fakeStore: &fakeStore{stream: &fakeStream{}}, err: errors.New("not authorized on outbox")}
	handler := &countingHandler{}

	var delivered int
	r := mustRelay(t, st, senderFunc(func(context.Context, *event.Metadata, []byte) error {
		delivered++
		return nil
	}), stream.WithDrainWindow(time.Millisecond), stream.WithLeaseTTL(time.Second),
		stream.WithOpTimeout(500*time.Millisecond),
		stream.WithLogger(slog.New(handler)))

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	if err := r.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v, want the ctx error: a failed EnsureIndexes must not stop delivery", err)
	}
	if !handler.logged("stream relay: could not ensure the store's retention indexes; " +
		"the outbox collection may grow without bound until this is resolved") {
		t.Fatalf("the failed EnsureIndexes was not logged (records: %v)", handler.messages())
	}
}

// --- event.ErrUnsendable ---------------------------------------------------------

// TestUnsendableMetadataIsParkedWithoutAClassifier pins the built-in permanent
// class the transports themselves declare.
//
// event.ErrUnsendable marks metadata no transport can serialize — a dot-less
// event type, a reserved extension name, a value the wire format cannot carry.
// Publish-time validation keeps such metadata out of the store, but an outbox row
// is DURABLE: rows written before those checks existed still reach the sender.
// Without the marker their rejection is an opaque error no classifier claims, so
// the lane stops on that row every tick forever with nothing behind it delivered
// — and requiring every caller to hand-write a WithUnsendableClassifier for a
// failure mode this library itself produces is not a contract anyone can be
// expected to discover before the outage.
func TestUnsendableMetadataIsParkedWithoutAClassifier(t *testing.T) {
	st := &fakeStore{stream: &fakeStream{events: []*stream.Event{ev("a"), ev("bad"), ev("c")}}}

	unsendable := fmt.Errorf("%w: malformed event type \"nodots\"", event.ErrUnsendable)
	var delivered []string
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		if md.ID == "bad" {
			return unsendable
		}
		delivered = append(delivered, md.ID)
		return nil
	})

	var parked []string
	// No WithUnsendableClassifier: the marker alone must be enough.
	r := mustRelay(t, st, sender, stream.WithPoisonHandler(
		func(_ context.Context, m *outbox.Message, _ error) error {
			parked = append(parked, m.ID)
			return nil
		}))

	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(parked) != 1 || parked[0] != "bad" {
		t.Fatalf("parked = %v, want [bad]", parked)
	}
	if len(delivered) != 2 || delivered[0] != "a" || delivered[1] != "c" {
		t.Fatalf("delivered = %v, want [a c] (the lane must advance past the unsendable event)", delivered)
	}
	if st.savedTok != "c" {
		t.Fatalf("savedTok = %q, want %q", st.savedTok, "c")
	}
}

// TestUnsendableMetadataStopsTheLaneWithNoPoisonHandler pins the other half:
// "permanent" authorizes advancing PAST the message, and with no PoisonHandler
// there is nowhere to put it, so advancing would silently drop the event. Stopping
// is what the runtime already does for an undecodable poison row, and this is the
// same situation from the other side of the wire.
func TestUnsendableMetadataStopsTheLaneWithNoPoisonHandler(t *testing.T) {
	st := &fakeStore{stream: &fakeStream{events: []*stream.Event{ev("a"), ev("bad")}}}

	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		if md.ID == "bad" {
			return fmt.Errorf("%w: malformed event type", event.ErrUnsendable)
		}
		return nil
	})

	r := mustRelay(t, st, sender)

	if err := r.RunOnce(t.Context()); !errors.Is(err, stream.ErrLaneStopped) {
		t.Fatalf("RunOnce = %v, want ErrLaneStopped (nowhere to park means the event must not be skipped)", err)
	}
	if st.savedTok == "bad" {
		t.Fatal("resume token advanced past an unparked event: it is now permanently skipped")
	}
}

// TestBaselineIsDroppedOnceAPositionIsPersisted pins that the in-memory fresh-group
// baseline stops being consulted the moment the store actually holds a position.
//
// The baseline exists to survive a FAILED first save. It is not cleared once a save
// succeeds, and RunOnce consults it whenever LoadToken returns "" — so it also
// resurrects itself in two situations where it is actively wrong:
//
//   - the documented break-glass DeleteToken (mongodb store: "the next Watch starts
//     at now"). If the deleted token is the one that fell off the oplog, reopening
//     from the stale baseline re-enters ErrHistoryLost forever and the documented
//     recovery cannot work.
//   - a decommissioned-then-revived group, which must start at "now" and instead
//     rewinds to a position from the previous incarnation.
//
// Reachable whenever the same *Relay value outlives the store row: a supervisor
// re-invoking Run, or a caller driving RunOnce directly (which ErrLaneStopped's
// contract explicitly supports).
func TestBaselineIsDroppedOnceAPositionIsPersisted(t *testing.T) {
	// One event so a send failure stops the lane, which is what closes the stream and
	// forces the reopen this test is about — no test-only API needed.
	fs := &fakeStream{
		events: []*stream.Event{ev("a")},
		pbrt:   "baseline-tok", pbrtCT: time.Unix(1000, 0).UTC(),
	}
	st := &fakeStore{stream: fs, loadTok: ""}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error {
		return errors.New("broker down")
	})

	r := mustRelay(t, st, sender,
		stream.WithDrainWindow(5*time.Millisecond), stream.WithLeaseTTL(time.Second),
		stream.WithOpTimeout(500*time.Millisecond))

	// Fresh group: takes the baseline, persists it, then the lane stops on the send
	// failure and the stream is closed.
	if err := r.RunOnce(t.Context()); !errors.Is(err, stream.ErrLaneStopped) {
		t.Fatalf("first RunOnce = %v, want ErrLaneStopped", err)
	}
	st.mu.Lock()
	saved := st.savedTok
	st.mu.Unlock()
	if saved != "baseline-tok" {
		t.Fatalf("baseline was not persisted (savedTok=%q); this test assumes it was", saved)
	}

	// The operator runs the break-glass DeleteToken: the row is gone, and the
	// documented contract is that the next Watch starts at "now".
	st.mu.Lock()
	st.loadTok = ""
	st.mu.Unlock()

	_ = r.RunOnce(t.Context())

	st.mu.Lock()
	toks := slices.Clone(st.watchTokens)
	st.mu.Unlock()

	last := toks[len(toks)-1]
	if last != "" {
		t.Fatalf("Watch token after DeleteToken = %q, want \"\" (start at now): the stale in-memory "+
			"baseline outlived the position it was a fallback for, so the documented break-glass "+
			"recovery reopens at the very token it was run to escape. watchTokens=%q", last, toks)
	}
}

// TestStaleTokenSaveReachesTheObserver pins that the one in-band signal of a
// split-brain leader is not log-only.
//
// SaveToken returning persisted=false means this relay delivered a window whose
// position another writer had already moved past — i.e. those events were very likely
// delivered twice by two leaders. Every other signal reports success: OnDrained
// counts the sends, OnLeadership says leader, the pass returns nil. So a deployment
// that scrapes metrics and ships logs elsewhere (or discards them, which WithLogger
// explicitly permits) had no way to know a dual-leader episode happened at all.
//
// This is the same shape as the sweep hole: the condition IS detected, and the
// detection reaches only a channel the alarm does not watch.
func TestStaleTokenSaveReachesTheObserver(t *testing.T) {
	fs := &fakeStream{events: []*stream.Event{ev("a"), ev("b")}}
	st := &fakeStore{
		stream: fs,
		// An established group, so the fresh-group baseline path is not what is
		// being exercised here.
		loadTok: "existing-tok",
		// The store's monotone guard rejects the save: a newer position is stored.
		denyPersist: true,
	}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })

	var observed []error
	obs := relay.Observer{OnError: func(_ string, err error) {
		observed = append(observed, err)
	}}

	r := mustRelay(t, st, sender,
		stream.WithObserver(obs),
		stream.WithDrainWindow(5*time.Millisecond), stream.WithLeaseTTL(time.Second),
		stream.WithOpTimeout(500*time.Millisecond))

	// The pass itself must still succeed: the store's guard did its job, and failing
	// the pass would close and reopen the stream over a condition the next
	// TryAcquire resolves anyway.
	if err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce = %v, want nil (a rejected stale save is not a pass failure)", err)
	}

	if len(observed) == 0 {
		t.Fatal("a rejected stale resume-token save reported NOTHING to the Observer: a " +
			"metrics-only deployment cannot tell a split-brain leader from a healthy one, " +
			"because OnDrained, OnLeadership and the pass result all report success")
	}
	found := false
	for _, err := range observed {
		if strings.Contains(err.Error(), "stale") {
			found = true

			break
		}
	}
	if !found {
		t.Fatalf("Observer saw %v, want an error identifying the stale/rejected token save", observed)
	}
}
