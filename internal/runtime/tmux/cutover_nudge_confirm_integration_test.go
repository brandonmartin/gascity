//go:build integration

package tmux

import (
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// These tests drive the SEAM-BACKED provider — NewSeamBackedWithConfig, the
// constructor cmd/gc's runtime registry actually builds for tmux — rather than
// the raw provider. That distinction is the whole point.
//
// ga-287 shipped a fix that carried tmux's submit observation up through the raw
// provider, the CLI's statusProvider wrapper, the auto/hybrid routers, the
// session manager and both worker handles. It kept recurring in the field on a
// binary that contained all of it, because production does not use the raw
// provider: it uses the seam-backed one, whose NudgeConfirm routes through the
// seam adapter. The seam adapter had no way to express a submit observation, so
// the wrapper fell through to "cannot confirm, report delivered" and threw the
// runtime's verdict away one layer below every test that existed.
//
// A unit test cannot catch that: it is a wiring gap between two real components,
// and both halves look correct in isolation. Only exercising the production
// constructor against a real pane that never starts a turn proves the signal
// survives the trip.

// seamBackedTestProvider builds the production tmux provider on the test socket.
func seamBackedTestProvider(idleTimeout time.Duration) runtime.Provider {
	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	cfg.NudgeIdleTimeout = idleTimeout
	return NewSeamBackedWithConfig(cfg)
}

// startNeverBusyClaudePane starts a fake claude TUI that accepts pasted text but
// never starts a turn, no matter how many Enters arrive — the turn-exited pane
// that swallows the submit and leaves the nudge drafted in the input box.
func startNeverBusyClaudePane(t *testing.T, label string) (*Tmux, string) {
	t.Helper()
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	dir := t.TempDir()
	fake := buildBusyOnEnterBinary(t, dir, "fakeclaude")
	sessionName := fmt.Sprintf("gt-test-seam-%s-%d", label, time.Now().UnixNano()%100000)

	_ = tm.KillSession(sessionName)
	if err := tm.NewSessionWithCommandAndEnv(sessionName, dir, fake, map[string]string{
		"GC_PROVIDER": "claude",
		// Higher than submitEnterMaxSends, so no Enter this path can send ever
		// drives the agent busy.
		"GC_TEST_BUSY_AFTER": "99",
		// Print an idle prompt so WaitForIdle resolves promptly and the test
		// measures the submit confirmation, not the idle wait.
		"GC_TEST_IDLE_PROMPT": "1",
	}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(sessionName) })
	time.Sleep(300 * time.Millisecond)
	return tm, sessionName
}

// confirmingSeamBackedProvider asserts the production provider exposes the
// confirm pair at all. Before the fix it did — the wrapper defined both methods
// — but they were structurally unable to return false, so this assertion alone
// is not the guard; the behavioral checks below are.
func confirmingSeamBackedProvider(t *testing.T, sp runtime.Provider) runtime.ConfirmingNudgeProvider {
	t.Helper()
	cp, ok := sp.(runtime.ConfirmingNudgeProvider)
	if !ok {
		t.Fatal("seam-backed tmux provider does not implement runtime.ConfirmingNudgeProvider")
	}
	return cp
}

// TestSeamBackedNudgeConfirmReportsUnsubmittedDraft is the regression guard for
// the deployed-binary gap: wait-idle delivery through the production provider
// must report the unsubmitted draft, which is what makes the CLI's
// wait-idle -> queue fallback reachable.
func TestSeamBackedNudgeConfirmReportsUnsubmittedDraft(t *testing.T) {
	tm, sessionName := startNeverBusyClaudePane(t, "waitidle")
	cp := confirmingSeamBackedProvider(t, seamBackedTestProvider(2*time.Second))

	submitted, err := cp.NudgeConfirm(sessionName, runtime.TextContent("keep patrolling"))
	if err != nil {
		t.Fatalf("NudgeConfirm: %v", err)
	}
	if submitted {
		out, _ := tm.CapturePaneAll(sessionName)
		t.Fatalf("seam-backed NudgeConfirm reported submitted=true for a pane that never started a turn.\n"+
			"The submit observation is being discarded at the seam boundary, so every wait-idle nudge\n"+
			"reads as delivered and the queue fallback never runs (ga-287).\npane:\n%s", out)
	}
}

// TestSeamBackedNudgeNowConfirmReportsUnsubmittedDraft is the same guard for
// immediate delivery — the mode operators use to recover a stranded agent, and
// the one whose result the CLI reports as a non-zero exit rather than queueing.
func TestSeamBackedNudgeNowConfirmReportsUnsubmittedDraft(t *testing.T) {
	tm, sessionName := startNeverBusyClaudePane(t, "immediate")
	cp := confirmingSeamBackedProvider(t, seamBackedTestProvider(2*time.Second))

	submitted, err := cp.NudgeNowConfirm(sessionName, runtime.TextContent("keep patrolling"))
	if err != nil {
		t.Fatalf("NudgeNowConfirm: %v", err)
	}
	if submitted {
		out, _ := tm.CapturePaneAll(sessionName)
		t.Fatalf("seam-backed NudgeNowConfirm reported submitted=true for a pane that never started a turn;\n"+
			"`gc session nudge --delivery immediate` would exit 0 on a message that never ran.\npane:\n%s", out)
	}
}

// TestSeamBackedNudgeConfirmReportsLandedSubmit pins the happy path through the
// production provider: an agent that does start a turn still reads as delivered.
// Without this, "always report false" would pass the guards above while
// downgrading every healthy nudge in the town to a queued redelivery.
func TestSeamBackedNudgeConfirmReportsLandedSubmit(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	dir := t.TempDir()
	fake := buildBusyOnEnterBinary(t, dir, "fakeclaude")
	sessionName := fmt.Sprintf("gt-test-seam-landed-%d", time.Now().UnixNano()%100000)

	_ = tm.KillSession(sessionName)
	if err := tm.NewSessionWithCommandAndEnv(sessionName, dir, fake, map[string]string{
		"GC_PROVIDER": "claude",
		// Go busy on the first Enter: a submit that lands.
		"GC_TEST_BUSY_AFTER":  "1",
		"GC_TEST_IDLE_PROMPT": "1",
	}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	time.Sleep(300 * time.Millisecond)

	cp := confirmingSeamBackedProvider(t, seamBackedTestProvider(2*time.Second))
	submitted, err := cp.NudgeConfirm(sessionName, runtime.TextContent("keep patrolling"))
	if err != nil {
		t.Fatalf("NudgeConfirm: %v", err)
	}
	if !submitted {
		out, _ := tm.CapturePaneAll(sessionName)
		t.Fatalf("seam-backed NudgeConfirm reported submitted=false for a pane that DID start a turn;\n"+
			"honest reporting must not downgrade working delivery into needless queueing.\npane:\n%s", out)
	}
}
