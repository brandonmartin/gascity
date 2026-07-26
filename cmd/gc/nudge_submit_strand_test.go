package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ga-287: wait-idle is the default delivery mode, and its failure state was
// silent. Against an agent that had already exited its turn, the text landed in
// the input box but the submit did not, so the agent sat parked on a drafted
// line holding open work. The nudge reported success, the session kept reading
// active, and nothing retried — three rigs' patrol coverage went dark for ~40
// minutes with no alarm anywhere.
//
// The fix is that an unsubmitted draft is not a delivery. wait-idle already
// falls back to the queue when delivery does not happen, so reporting the
// submit state honestly is what makes the dispatcher retry at the next idle
// boundary instead of dropping the work.

func TestDeliverSessionNudgeWaitIdleQueuesWhenSubmitNeverLands(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Idle target: the agent has exited its turn, so wait-idle proceeds to
	// inject. The strand happens on the submit that follows.
	fake.WaitForIdleErrors["sess-worker"] = nil
	fake.SetNudgeSubmitUnconfirmed("sess-worker", true)

	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "worker"},
		resolved:    &config.ResolvedProvider{Name: "claude"},
		sessionName: "sess-worker",
	}

	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeWithProvider(target, fake, nudgeDeliveryWaitIdle, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("deliverSessionNudgeWithProvider = %d, want 0; stderr: %s", code, stderr.String())
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending=%d inFlight=%d dead=%d, want the stranded nudge queued for redelivery", len(pending), len(inFlight), len(dead))
	}
	if !strings.Contains(stdout.String()+stderr.String(), "ueue") {
		t.Fatalf("output = %q/%q, want the caller told the nudge was queued", stdout.String(), stderr.String())
	}
}

// The healthy path must stay a live delivery — the fix must not push every
// wait-idle nudge onto the queue.
func TestDeliverSessionNudgeWaitIdleStaysLiveWhenSubmitLands(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.WaitForIdleErrors["sess-worker"] = nil
	fake.SetNudgeSubmitUnconfirmed("sess-worker", false)

	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "worker"},
		resolved:    &config.ResolvedProvider{Name: "claude"},
		sessionName: "sess-worker",
	}

	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeWithProvider(target, fake, nudgeDeliveryWaitIdle, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("deliverSessionNudgeWithProvider = %d, want 0; stderr: %s", code, stderr.String())
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending=%d inFlight=%d dead=%d, want all zero for a confirmed submit", len(pending), len(inFlight), len(dead))
	}
}

// --delivery immediate is the operator's recovery tool for an already-parked
// agent. It has no queue to fall back to, so a strand there must surface as a
// failure rather than an announced delivery — a silent "Nudged" is what sends
// the caller away believing a parked peer was recovered.
func TestDeliverSessionNudgeImmediateReportsUnsubmittedDraft(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.SetNudgeSubmitUnconfirmed("sess-worker", true)

	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "worker"},
		resolved:    &config.ResolvedProvider{Name: "claude"},
		sessionName: "sess-worker",
	}

	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeWithProvider(target, fake, nudgeDeliveryImmediate, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("deliverSessionNudgeWithProvider = 0 for an unsubmitted draft, want non-zero; stdout: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "Nudged") {
		t.Fatalf("stdout = %q, want no delivery announcement for an unsubmitted draft", stdout.String())
	}
	if !strings.Contains(stderr.String(), "drafted") {
		t.Fatalf("stderr = %q, want the drafted-not-run diagnosis", stderr.String())
	}
}
