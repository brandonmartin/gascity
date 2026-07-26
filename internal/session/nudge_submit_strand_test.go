package session

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ga-287: typing the nudge and submitting it are separate keystrokes on a
// terminal runtime. When the submit is lost the text sits drafted in the
// agent's input box — the session keeps reporting active and the runtime
// reports no error, so a manager that answered "delivered" here made the
// stranded work invisible to every caller above it.
//
// These pin the wait-idle manager path (the one behind SessionHandle) to
// report the SUBMIT state, so the CLI's queue fallback engages.

func newStrandTestManager(t *testing.T, sessionName string) (*Manager, *runtime.Fake) {
	t.Helper()
	b := sessionBeadFixture(sessionName, "open", map[string]string{
		"__title":      "Strand",
		"template":     "worker",
		"state":        "active",
		"provider":     "claude",
		"session_name": sessionName,
	})
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.WaitForIdleErrors[sessionName] = nil
	return NewManagerWithOptions(beads.NewMemStoreFrom(1, []beads.Bead{b}, nil), fake), fake
}

func TestTryWaitIdleNudgeReportsUndeliveredWhenSubmitNeverLands(t *testing.T) {
	const name = "s-strand-1"
	mgr, fake := newStrandTestManager(t, name)
	fake.SetNudgeSubmitUnconfirmed(name, true)

	delivered, err := mgr.TryWaitIdleNudge(context.Background(), name, "session", "keep patrolling", "", runtime.Config{})
	if err != nil {
		t.Fatalf("TryWaitIdleNudge: %v", err)
	}
	if delivered {
		t.Fatal("TryWaitIdleNudge delivered = true for an unsubmitted draft, want false so the caller queues a redelivery")
	}
}

func TestTryWaitIdleNudgeStaysDeliveredWhenSubmitLands(t *testing.T) {
	const name = "s-strand-2"
	mgr, fake := newStrandTestManager(t, name)
	fake.SetNudgeSubmitUnconfirmed(name, false)

	delivered, err := mgr.TryWaitIdleNudge(context.Background(), name, "session", "keep patrolling", "", runtime.Config{})
	if err != nil {
		t.Fatalf("TryWaitIdleNudge: %v", err)
	}
	if !delivered {
		t.Fatal("TryWaitIdleNudge delivered = false for a confirmed submit, want true")
	}
}

func TestTryWaitIdleNudgeLiveOnlyReportsUndeliveredWhenSubmitNeverLands(t *testing.T) {
	const name = "s-strand-3"
	mgr, fake := newStrandTestManager(t, name)
	fake.SetNudgeSubmitUnconfirmed(name, true)

	delivered, err := mgr.TryWaitIdleNudgeLiveOnly(context.Background(), name, "session", "keep patrolling")
	if err != nil {
		t.Fatalf("TryWaitIdleNudgeLiveOnly: %v", err)
	}
	if delivered {
		t.Fatal("TryWaitIdleNudgeLiveOnly delivered = true for an unsubmitted draft, want false")
	}
}

// The queued dispatcher redelivers through SendLiveOnly and acks the queue
// entry on a true result. Acking an unsubmitted draft would drop the nudge for
// good, so this path must report the submit state too.
func TestSendLiveOnlyReportsUndeliveredWhenSubmitNeverLands(t *testing.T) {
	const name = "s-strand-4"
	mgr, fake := newStrandTestManager(t, name)
	fake.SetNudgeSubmitUnconfirmed(name, true)

	delivered, err := mgr.SendLiveOnly(context.Background(), name, "keep patrolling")
	if err != nil {
		t.Fatalf("SendLiveOnly: %v", err)
	}
	if delivered {
		t.Fatal("SendLiveOnly delivered = true for an unsubmitted draft, want false so the queue claim is released and retried")
	}
}
