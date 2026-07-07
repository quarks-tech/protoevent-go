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
}

func (s *fakeStream) Next(context.Context) (*stream.Event, bool, error) {
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
	mu         sync.Mutex
	stream     *fakeStream
	loadTok    string
	loadCT     time.Time
	savedTok   string
	savedCT    time.Time
	saveCount  int
	watchCount int
	leader     string
}

func (s *fakeStreamStore) LoadToken(context.Context, string) (string, time.Time, error) {
	return s.loadTok, s.loadCT, nil
}
func (s *fakeStreamStore) SaveToken(_ context.Context, _ string, tok string, ct time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.savedTok, s.savedCT, s.saveCount = tok, ct, s.saveCount+1
	return nil
}
func (s *fakeStreamStore) Watch(context.Context, string) (stream.Stream, error) {
	s.mu.Lock()
	s.watchCount++
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
	if err := r.RunOnce(context.Background()); err != nil {
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
	if err := r.RunOnce(context.Background()); err != nil {
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
	_ = r.RunOnce(context.Background())
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
	err := r.RunOnce(context.Background())
	if !errors.Is(err, stream.ErrStreamInvalidated) {
		t.Fatalf("err = %v, want ErrStreamInvalidated", err)
	}
}

func TestRunOnceNonLeaderIdles(t *testing.T) {
	st := &fakeStreamStore{stream: &fakeStream{events: []*stream.Event{ev(1, "a", false)}}, leader: "other"}
	sent := 0
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { sent++; return nil })
	r, _ := stream.NewRelay("c", st, sender, stream.WithLeaderLockName("lock"))
	if err := r.RunOnce(context.Background()); err != nil {
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
	_ = r.RunOnce(context.Background())

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
	if err := r.RunOnce(context.Background()); err != nil {
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
