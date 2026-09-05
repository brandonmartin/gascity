package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestLogPoolTriggerWorktreeEvidenceSkipEmitsDistinctivePrefix pins the
// user-facing shape of the pool-starvation notice added for ga-cix. The old
// stderr line ("buildDesiredState: pool ...: %v (skipping)") blended into
// per-tick chatter, and three ready L/XL beads sat unclaimed for 7+ hours
// while pool starvation went unnoticed. The new line must be greppable by a
// distinctive tag AND name the work bead operators need to remediate.
func TestLogPoolTriggerWorktreeEvidenceSkipEmitsDistinctivePrefix(t *testing.T) {
	resetPoolTriggerSkipNoticeSeenForTest()
	var stderr bytes.Buffer
	err := fmt.Errorf("%w: bead %q does not match request bead %q", errPoolTriggerWorktreeEvidence, "ga-old", "ga-new")

	logPoolTriggerWorktreeEvidenceSkip(&stderr, "rig/pool", "sess-1", "ga-new", err)

	got := stderr.String()
	if !strings.HasPrefix(got, "POOL-STARVATION-SKIP:") {
		t.Fatalf("stderr = %q, want it to start with the distinctive tag %q", got, "POOL-STARVATION-SKIP:")
	}
	if !strings.Contains(got, "ga-new") {
		t.Fatalf("stderr = %q, want it to name the work bead id %q so operators can inspect it", got, "ga-new")
	}
	if !strings.Contains(got, "rig/pool") {
		t.Fatalf("stderr = %q, want it to name the pool %q", got, "rig/pool")
	}
	if !strings.Contains(got, "sess-1") {
		t.Fatalf("stderr = %q, want it to name the session %q", got, "sess-1")
	}
	if !strings.Contains(got, "bd show ga-new") {
		t.Fatalf("stderr = %q, want an actionable 'bd show <id>' hint so the operator has one command to run", got)
	}
}

// TestLogPoolTriggerWorktreeEvidenceSkipDedupsIdenticalHits confirms the
// per-tick spam guard: buildDesiredState reaches this site every reconciler
// tick as long as the bead's evidence stays bad, so an unguarded log line
// would flood the controller log. Two identical calls must produce exactly
// one line.
func TestLogPoolTriggerWorktreeEvidenceSkipDedupsIdenticalHits(t *testing.T) {
	resetPoolTriggerSkipNoticeSeenForTest()
	var stderr bytes.Buffer
	err := fmt.Errorf("%w: verification failed", errPoolTriggerWorktreeEvidence)

	logPoolTriggerWorktreeEvidenceSkip(&stderr, "rig/pool", "sess-1", "ga-new", err)
	logPoolTriggerWorktreeEvidenceSkip(&stderr, "rig/pool", "sess-1", "ga-new", err)

	got := stderr.String()
	if count := strings.Count(got, "POOL-STARVATION-SKIP:"); count != 1 {
		t.Fatalf("stderr = %q, saw %d notices, want exactly 1 (per-tick dedup)", got, count)
	}
}

// TestLogPoolTriggerWorktreeEvidenceSkipReLogsOnNewErrorMessage confirms that
// remediation progress is visible: if the underlying error message changes
// (e.g. verification failure -> bead mismatch), the new failure mode logs
// again. A remedy that partially works but exposes a different problem must
// not be masked by an over-eager dedup.
func TestLogPoolTriggerWorktreeEvidenceSkipReLogsOnNewErrorMessage(t *testing.T) {
	resetPoolTriggerSkipNoticeSeenForTest()
	var stderr bytes.Buffer
	err1 := fmt.Errorf("%w: verification failed", errPoolTriggerWorktreeEvidence)
	err2 := fmt.Errorf("%w: bead mismatch", errPoolTriggerWorktreeEvidence)

	logPoolTriggerWorktreeEvidenceSkip(&stderr, "rig/pool", "sess-1", "ga-new", err1)
	logPoolTriggerWorktreeEvidenceSkip(&stderr, "rig/pool", "sess-1", "ga-new", err2)

	got := stderr.String()
	if count := strings.Count(got, "POOL-STARVATION-SKIP:"); count != 2 {
		t.Fatalf("stderr = %q, saw %d notices, want 2 (distinct error messages must both surface)", got, count)
	}
}

// TestLogPoolTriggerWorktreeEvidenceSkipIsNilSafe guards the two nil-input
// paths so callers never crash on a partially-wired build (nil stderr from a
// dry-run fixture; nil err from a defensive caller). Silence is the correct
// behavior for both.
func TestLogPoolTriggerWorktreeEvidenceSkipIsNilSafe(t *testing.T) {
	resetPoolTriggerSkipNoticeSeenForTest()
	// nil stderr: must not panic, must not populate dedup cache.
	logPoolTriggerWorktreeEvidenceSkip(nil, "rig/pool", "sess-1", "ga-new", errors.New("boom"))
	// nil err: must not panic, must not populate dedup cache.
	var stderr bytes.Buffer
	logPoolTriggerWorktreeEvidenceSkip(&stderr, "rig/pool", "sess-1", "ga-new", nil)
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty on nil err", stderr.String())
	}
}
