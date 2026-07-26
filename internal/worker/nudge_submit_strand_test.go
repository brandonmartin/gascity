package worker

import (
	"context"
	"strings"
	"testing"
)

// A terminal runtime types the nudge text and submits it as two separate
// keystrokes. When the submit is lost the text sits drafted in the agent's
// input box: the session keeps reporting active, the runtime reports no error,
// and the work the nudge carried never runs. Recording that as a delivery is
// what made ga-287 silent — three rigs' patrol coverage parked on drafted text
// for ~40 minutes with every status surface reading healthy.
//
// These tests pin the contract that an unconfirmed submit is reported as
// undelivered, so callers with a fallback (queued redelivery) can take it.

func TestNudgeWaitIdleReportsUndeliveredWhenSubmitNeverLands(t *testing.T) {
	handle, _, sp, mgr := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	})

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := mgr.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("manager.Get(%q): %v", handle.sessionID, err)
	}
	// Idle target: wait-idle proceeds straight to injection. The strand happens
	// after that, on the submit — an idle agent is exactly the parked,
	// turn-exited case, not a busy one.
	sp.WaitForIdleErrors[info.SessionName] = nil
	sp.SetNudgeSubmitUnconfirmed(info.SessionName, true)

	startCalls := len(sp.Calls)
	result, err := handle.Nudge(context.Background(), NudgeRequest{
		Text:     "keep patrolling",
		Delivery: NudgeDeliveryWaitIdle,
		Source:   "mail",
	})
	if err != nil {
		t.Fatalf("Nudge(wait_idle): %v", err)
	}
	if result.Delivered {
		t.Fatal("Nudge(wait_idle) Delivered = true for an unsubmitted draft, want false so the caller falls back to queued redelivery")
	}

	// The text still reached tmux, so the caller must not re-paste blindly —
	// the injection attempt is expected to have happened exactly once.
	calls := sp.Calls[startCalls:]
	nudges := 0
	for _, call := range calls {
		if call.Method == "NudgeNow" || call.Method == "Nudge" {
			nudges++
		}
	}
	if nudges != 1 {
		t.Fatalf("injection calls = %d, want exactly 1 (%#v)", nudges, calls)
	}
	nudge := firstCall(calls, "NudgeNow")
	if nudge == nil {
		t.Fatalf("calls = %#v, want NudgeNow", calls)
	}
	if !strings.Contains(nudge.Message, "keep patrolling") {
		t.Fatalf("delivered message = %q, want the nudge body", nudge.Message)
	}
}

func TestNudgeImmediateReportsUndeliveredWhenSubmitNeverLands(t *testing.T) {
	handle, _, sp, mgr := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	})

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := mgr.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("manager.Get(%q): %v", handle.sessionID, err)
	}
	sp.SetNudgeSubmitUnconfirmed(info.SessionName, true)

	result, err := handle.Nudge(context.Background(), NudgeRequest{
		Text:     "run gc hook",
		Delivery: NudgeDeliveryImmediate,
		Source:   "session",
	})
	if err != nil {
		t.Fatalf("Nudge(immediate): %v", err)
	}
	if result.Delivered {
		t.Fatal("Nudge(immediate) Delivered = true for an unsubmitted draft, want false")
	}
}

// A runtime that CAN confirm and did observe the submit must stay a plain
// delivery — the fix must not downgrade the healthy path.
func TestNudgeWaitIdleStaysDeliveredWhenSubmitLands(t *testing.T) {
	handle, _, sp, mgr := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	})

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := mgr.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("manager.Get(%q): %v", handle.sessionID, err)
	}
	sp.WaitForIdleErrors[info.SessionName] = nil
	sp.SetNudgeSubmitUnconfirmed(info.SessionName, false)

	result, err := handle.Nudge(context.Background(), NudgeRequest{
		Text:     "keep patrolling",
		Delivery: NudgeDeliveryWaitIdle,
		Source:   "mail",
	})
	if err != nil {
		t.Fatalf("Nudge(wait_idle): %v", err)
	}
	if !result.Delivered {
		t.Fatal("Nudge(wait_idle) Delivered = false for a confirmed submit, want true")
	}
}
