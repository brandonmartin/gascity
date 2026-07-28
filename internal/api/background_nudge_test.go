package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/worker"
)

// The background-message path is the one nudge caller in the codebase with no
// queued-redelivery fallback: everything routed through cmd/gc (sling, mail
// notify, session nudge, the queue dispatcher) enqueues when a nudge comes back
// undelivered, but an external-message wake has nowhere to fall back to. That
// makes honest reporting the ONLY thing standing between a stranded submit and
// a silently lost message.
//
// Before ga-8tno this path discarded NudgeResult.Delivered entirely, so a
// submit the runtime had explicitly observed to strand — text drafted in the
// agent's input box, turn never started — returned nil and the caller's error
// log stayed empty. The strand was detected and then dropped on the floor.

func TestBackgroundNudgeOutcomeReportsUnsubmittedDraft(t *testing.T) {
	err := backgroundNudgeOutcome("th-abc12", worker.NudgeResult{Delivered: false}, nil)
	if err == nil {
		t.Fatal("backgroundNudgeOutcome returned nil for a nudge the runtime never saw submit; the message is drafted in the agent's input box and nothing reports it (ga-8tno)")
	}
	if !errors.Is(err, errBackgroundNudgeUnsubmitted) {
		t.Fatalf("error = %v, want one matching errBackgroundNudgeUnsubmitted so callers can tell a strand from a transport failure", err)
	}
	if !strings.Contains(err.Error(), "th-abc12") {
		t.Fatalf("error %q does not name the session; the caller logs this and needs to know which agent is parked", err)
	}
}

func TestBackgroundNudgeOutcomeAcceptsDeliveredNudge(t *testing.T) {
	if err := backgroundNudgeOutcome("th-abc12", worker.NudgeResult{Delivered: true}, nil); err != nil {
		t.Fatalf("backgroundNudgeOutcome = %v, want nil for a submit the runtime confirmed landed", err)
	}
}

// A transport failure and a stranded submit need different recovery — one
// never reached the runtime, the other is sitting in the input box — so the
// original cause must survive rather than being flattened into the strand
// sentinel.
func TestBackgroundNudgeOutcomeSurfacesTransportError(t *testing.T) {
	sentinel := errors.New("session gone")
	err := backgroundNudgeOutcome("th-abc12", worker.NudgeResult{}, sentinel)
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want %v", err, sentinel)
	}
	if errors.Is(err, errBackgroundNudgeUnsubmitted) {
		t.Fatal("a transport failure was reported as a stranded draft; the two need different recovery")
	}
}
