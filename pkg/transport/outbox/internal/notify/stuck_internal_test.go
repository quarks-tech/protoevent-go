package notify

import (
	"testing"
	"time"
)

// backdate makes the open episode look old enough to escalate, standing in for
// the wall-clock wait the threshold otherwise requires.
func backdate[P comparable](s *StuckTracker[P]) {
	s.since = time.Now().Add(-2 * StuckEscalateAfter)
}

// TestStuckTrackerEscalatesOncePerEpisode pins the base contract the escalation
// exists for: one alert per wedge, not one per pass.
func TestStuckTrackerEscalatesOncePerEpisode(t *testing.T) {
	var s StuckTracker[string]

	if _, escalate := s.Stuck("tok", true); escalate {
		t.Fatal("escalated on the first stop; want the threshold to apply")
	}

	backdate(&s)

	if _, escalate := s.Stuck("tok", true); !escalate {
		t.Fatal("did not escalate past the threshold")
	}

	backdate(&s)

	if _, escalate := s.Stuck("tok", true); escalate {
		t.Fatal("escalated twice in one episode; want exactly one alert per wedge")
	}
}

// TestStuckTrackerReEscalatesAfterProgress pins that ending an episode re-arms
// the tracker.
func TestStuckTrackerReEscalatesAfterProgress(t *testing.T) {
	var s StuckTracker[string]

	s.Stuck("tok", true)
	backdate(&s)

	if _, escalate := s.Stuck("tok", true); !escalate {
		t.Fatal("first wedge did not escalate")
	}

	// The lane moved past the wedged position: the episode is over.
	s.Progress("tok")

	s.Stuck("tok", true)
	backdate(&s)

	if _, escalate := s.Stuck("tok", true); !escalate {
		t.Fatal("second wedge was suppressed after the first episode ended")
	}
}

// TestStuckTrackerUnidentifiedEpisodeClearsOnAnyProgress pins the case that used
// to suppress every later alert for the process lifetime.
//
// The stream runtime keys an episode on the poison event's resume token, falling
// back to its id; when both extractions come back empty there is no key at all.
// No later Progress call can match such an episode (every delivered event carries
// at least one of the two), so it stayed active-and-escalated forever: after one
// unidentifiable wedge was cleared by an operator, the NEXT one — a genuinely new
// incident — produced no StuckLaneError and no log line. An episode reported as
// unidentified therefore ends on ANY successful disposal.
func TestStuckTrackerUnidentifiedEpisodeClearsOnAnyProgress(t *testing.T) {
	var s StuckTracker[string]

	s.Stuck("", false)
	backdate(&s)

	if _, escalate := s.Stuck("", false); !escalate {
		t.Fatal("first unidentifiable wedge did not escalate")
	}

	// Any disposal ends it — this one is at an unrelated position, which is all a
	// delivered event can ever offer.
	s.Progress("some-other-event")

	// A second, genuinely new unidentifiable wedge.
	s.Stuck("", false)
	backdate(&s)

	if _, escalate := s.Stuck("", false); !escalate {
		t.Fatal("second unidentifiable wedge was suppressed; the one alert that means a human is needed never fires again")
	}
}

// TestStuckTrackerProgressIgnoresOtherPositions pins the position check that the
// unidentified-episode clearing must not undermine: a pass re-delivering a prefix
// AHEAD of the wedged message (what a reopen does whenever the token save is being
// rejected) must not reset the timer, or a permanently wedged lane never reaches
// the threshold.
func TestStuckTrackerProgressIgnoresOtherPositions(t *testing.T) {
	var s StuckTracker[string]

	s.Stuck("wedged", true)
	backdate(&s)

	s.Progress("earlier") // a re-delivered prefix, not the wedge
	s.Progress("")        // an empty key must not match an identified episode either

	if _, escalate := s.Stuck("wedged", true); !escalate {
		t.Fatal("progress at an unrelated position reset the episode; a permanent wedge would never escalate")
	}
}

// TestStuckTrackerIdentifiedZeroPositionIsNotUnidentified pins that the zero
// position and "unidentified" are different things: seq 0 is a legitimate,
// identifiable position for the sequence runtime, and an episode there must NOT
// be cleared by progress somewhere else.
func TestStuckTrackerIdentifiedZeroPositionIsNotUnidentified(t *testing.T) {
	var s StuckTracker[int64]

	s.Stuck(0, true)
	backdate(&s)

	s.Progress(42) // progress elsewhere: the episode at seq 0 stands

	if _, escalate := s.Stuck(0, true); !escalate {
		t.Fatal("an episode at seq 0 was cleared by progress at another seq")
	}
}
