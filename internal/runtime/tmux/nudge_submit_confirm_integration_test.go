//go:build integration

package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// fakeAgentStartupSettle gives the fake agent TUI time to reach its input loop
// before a test drives it.
const fakeAgentStartupSettle = 300 * time.Millisecond

// startFakeAgentSession builds the fake agent TUI, starts it in a uniquely named
// tmux session with env, registers teardown, and waits for it to come up.
//
// It is the single owner of this file's session setup: the per-test copies were
// identical, and folding them here keeps one tmux and one fixed-sleep dependency
// for the whole file instead of one per test.
func startFakeAgentSession(t *testing.T, tm *Tmux, label string, env map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	fake := buildBusyOnEnterBinary(t, dir, "fakeclaude")
	sessionName := fmt.Sprintf("gt-test-%s-%d", label, time.Now().UnixNano()%100000)

	_ = tm.KillSession(sessionName)
	if err := tm.NewSessionWithCommandAndEnv(sessionName, dir, fake, env); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(sessionName) })
	time.Sleep(fakeAgentStartupSettle)
	return sessionName
}

// buildBusyOnEnterBinary compiles a fake agent TUI that echoes stdin and, after
// receiving GC_TEST_BUSY_AFTER Enter keystrokes (default 1), prints an
// "esc to interrupt" busy footer — the same signal paneContainsBusyIndicator
// uses to detect a live Claude turn. Each Enter also prints an "ENTER#<n>"
// marker so a test can assert exactly how many submit keystrokes were delivered.
//
// GC_TEST_STALE_BUSY_FILLER models a turn that has already finished: the busy
// footer is printed once at startup and then scrolled up out of the live footer
// region by that many filler rows, leaving it in scrollback where a pane capture
// still reaches it.
func buildBusyOnEnterBinary(t *testing.T, dir, name string) string {
	t.Helper()
	bin := dir + "/" + name
	src := dir + "/" + name + ".go"
	prog := `package main
import ("bufio";"fmt";"os";"strconv")
func main(){
	busyAfter:=1
	if v:=os.Getenv("GC_TEST_BUSY_AFTER"); v!=""{ if n,err:=strconv.Atoi(v); err==nil && n>0 { busyAfter=n } }
	if v:=os.Getenv("GC_TEST_STALE_BUSY_FILLER"); v!=""{
		if n,err:=strconv.Atoi(v); err==nil && n>0 {
			fmt.Print("esc to interrupt\n")
			for i:=0;i<n;i++{ fmt.Printf("transcript line %d\n", i) }
		}
	}
	if os.Getenv("GC_TEST_IDLE_PROMPT")!=""{ fmt.Print("❯ \n") }
	enters:=0
	r:=bufio.NewReader(os.Stdin)
	for{
		b,err:=r.ReadByte()
		if err!=nil{ return }
		if b=='\r'||b=='\n'{
			enters++
			fmt.Printf("\nENTER#%d\n", enters)
			if enters>=busyAfter { fmt.Print("esc to interrupt\n") }
			continue
		}
		_,_=os.Stdout.Write([]byte{b})
	}
}
`
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", src, err)
	}
	build := exec.Command("go", "build", "-o", bin, src)
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", name, err, string(out))
	}
	return bin
}

// TestNudgeSessionConfirmsSubmitForClaude proves the verified-submit path
// against real tmux: a single Enter that submits drives the fake agent busy,
// NudgeSession confirms it, and does NOT issue a redundant second Enter.
func TestNudgeSessionConfirmsSubmitForClaude(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	sessionName := startFakeAgentSession(t, tm, "nudge-confirm", map[string]string{
		"GC_PROVIDER":        "claude",
		"GC_TEST_BUSY_AFTER": "1",
	})

	if err := tm.NudgeSession(sessionName, "hello-confirm"); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}

	out, err := tm.CapturePaneAll(sessionName)
	if err != nil {
		t.Fatalf("CapturePaneAll: %v", err)
	}
	if !strings.Contains(out, "esc to interrupt") {
		t.Fatalf("pane never reached submitted/busy state:\n%s", out)
	}
	if strings.Contains(out, "ENTER#2") {
		t.Fatalf("issued a redundant Enter after the turn already submitted (double-submit):\n%s", out)
	}
}

// TestNudgeSessionReEntersUntilSubmittedForClaude proves the ga-bwm fix
// end-to-end on real tmux: when the first Enter is dropped (the draft stays in
// the input box), NudgeSession re-sends Enter, and the second submit drives the
// agent busy. Pre-fix, the message would sit "drafted but not submitted".
func TestNudgeSessionReEntersUntilSubmittedForClaude(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	sessionName := startFakeAgentSession(t, tm, "nudge-reenter", map[string]string{
		"GC_PROVIDER":        "claude",
		"GC_TEST_BUSY_AFTER": "2", // drop the first Enter, submit on the second
	})

	if err := tm.NudgeSession(sessionName, "hello-reenter"); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}

	out, err := tm.CapturePaneAll(sessionName)
	if err != nil {
		t.Fatalf("CapturePaneAll: %v", err)
	}
	if !strings.Contains(out, "ENTER#2") {
		t.Fatalf("did not re-send Enter after the first was dropped:\n%s", out)
	}
	if !strings.Contains(out, "esc to interrupt") {
		t.Fatalf("never reached submitted/busy state after re-send:\n%s", out)
	}
}

// TestNudgeSessionConfirmReportsUnsubmittedDraftForClaude proves the ga-287
// signal end-to-end on real tmux: an agent that never starts a turn leaves the
// message drafted in its input box, and NudgeSessionConfirm reports that as
// submitted=false with a nil error. The nil error is deliberate — the
// keystrokes DID reach tmux, so the caller must not re-paste blindly; it falls
// back to queued redelivery instead. Pre-fix this outcome was indistinguishable
// from a delivered nudge, which is how three rigs' patrol coverage sat parked
// on drafted text with every status surface reading healthy.
func TestNudgeSessionConfirmReportsUnsubmittedDraftForClaude(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	sessionName := startFakeAgentSession(t, tm, "nudge-strand", map[string]string{
		"GC_PROVIDER": "claude",
		// Higher than submitEnterMaxSends, so no Enter this path can send ever
		// drives the agent busy — the turn-exited pane that swallows the submit.
		"GC_TEST_BUSY_AFTER": "99",
	})

	submitted, err := tm.NudgeSessionConfirm(sessionName, "hello-strand")
	if err != nil {
		t.Fatalf("NudgeSessionConfirm: %v", err)
	}
	if submitted {
		out, _ := tm.CapturePaneAll(sessionName)
		t.Fatalf("NudgeSessionConfirm submitted = true for a never-busy pane, want false:\n%s", out)
	}

	out, err := tm.CapturePaneAll(sessionName)
	if err != nil {
		t.Fatalf("CapturePaneAll: %v", err)
	}
	if strings.Contains(out, "esc to interrupt") {
		t.Fatalf("pane reported busy; the never-busy fixture did not hold:\n%s", out)
	}
}

// TestNudgeSessionConfirmIgnoresStaleBusyFrameForClaude proves the ga-61tq fix
// end-to-end on real tmux: a pane whose scrollback still holds the busy footer
// of a turn that already finished must not confirm a submit that never landed.
//
// This is the hole the ga-287 reporting fix could not close on its own. Submit
// confirmation polls the pane's busy signal, and a pane capture reaches
// promptObservationLines into history — so the stale frame answered "busy" on
// the first poll. Confirmation came back true for a draft sitting untouched in
// the input box, and the Enter re-send that recovers a dropped submit never
// fired. Honest delivery reporting downstream cannot survive a lying probe.
//
// The fixture never goes busy on Enter, so the only busy text in the pane is
// the stale frame: a true result here is the false positive itself.
func TestNudgeSessionConfirmIgnoresStaleBusyFrameForClaude(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	sessionName := startFakeAgentSession(t, tm, "nudge-stale-busy", map[string]string{
		"GC_PROVIDER": "claude",
		// Never busy on Enter: the draft strands, exactly as the reported
		// turn-exited pane does.
		"GC_TEST_BUSY_AFTER": "99",
		// Scroll a finished turn's busy footer up out of the live footer region.
		"GC_TEST_STALE_BUSY_FILLER": "40",
	})

	out, err := tm.CapturePane(sessionName, promptObservationLines)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	if !strings.Contains(out, "esc to interrupt") {
		t.Fatalf("stale busy frame is not in the capture window; the fixture does not reproduce the false positive:\n%s", out)
	}

	submitted, err := tm.NudgeSessionConfirm(sessionName, "commit and run the done sequence")
	if err != nil {
		t.Fatalf("NudgeSessionConfirm: %v", err)
	}
	if submitted {
		pane, _ := tm.CapturePaneAll(sessionName)
		t.Fatalf("NudgeSessionConfirm submitted = true for a stranded draft; a finished turn's busy frame in scrollback confirmed a submit that never landed:\n%s", pane)
	}
}

// TestWaitForIdleIgnoresStaleBusyFrameForClaude covers the other half of ga-61tq
// on real tmux. wait-idle is the default delivery mode, and its first move is
// this wait. Reading a finished turn's busy frame out of scrollback as live
// activity holds the wait open against an agent already sitting at an idle
// prompt, so every wait-idle nudge to that session burns its full timeout before
// falling through to send anyway — the wait stops picking the moment to type and
// becomes pure latency.
func TestWaitForIdleIgnoresStaleBusyFrameForClaude(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	sessionName := startFakeAgentSession(t, tm, "idle-stale-busy", map[string]string{
		"GC_PROVIDER": "claude",
		// A finished turn's busy footer, scrolled up out of the live footer by
		// continued transcript output, with the agent now idle at its prompt.
		"GC_TEST_STALE_BUSY_FILLER": "40",
		"GC_TEST_IDLE_PROMPT":       "1",
		"GC_TEST_BUSY_AFTER":        "99",
	})

	out, err := tm.CapturePane(sessionName, promptObservationLines)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	if !strings.Contains(out, "esc to interrupt") {
		t.Fatalf("stale busy frame is not in the capture window; the fixture does not reproduce the false positive:\n%s", out)
	}

	if err := tm.WaitForIdle(context.Background(), sessionName, 3*time.Second); err != nil {
		t.Fatalf("WaitForIdle on an idle pane = %v, want nil; a finished turn's busy frame in scrollback held the wait open:\n%s", err, out)
	}
}

// TestSeamBackedNudgeConfirmRecoversStrandedSubmitForClaude proves the ga-8tno
// self-heal end-to-end on real tmux, through the constructor production uses
// (NewSeamBackedWithConfig — every routed nudge in the city goes through it).
//
// The fixture holds the agent idle for the whole of submitEnterAndConfirm's
// budget, so the first attempt strands exactly as the reported turn-exited pane
// does: text in the input box, turn never started, submitted=false. Every
// earlier fix stopped there — the strand was correctly DETECTED and then either
// dropped (callers with no queue) or re-run identically (callers with one), so
// recovery always needed a human to re-nudge.
//
// The next Enter after that budget is one only the seam adapter's retry can
// send, which makes "did the message ever run?" the assertion: a true result
// here means a stranded agent un-parked itself with nobody watching.
func TestSeamBackedNudgeConfirmRecoversStrandedSubmitForClaude(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	sessionName := startFakeAgentSession(t, tm, "seam-strand-recover", map[string]string{
		"GC_PROVIDER": "claude",
		// One more Enter than a single confirm attempt can send, so the first
		// attempt provably exhausts itself without the agent going busy.
		"GC_TEST_BUSY_AFTER": strconv.Itoa(submitEnterMaxSends + 1),
	})

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	// The fixture never shows an idle prompt, so the default wait would burn its
	// full window before typing. This test is about what happens AFTER the
	// submit strands, so keep the wait short rather than paying for it twice.
	cfg.NudgeIdleTimeout = time.Second
	sp := NewSeamBackedWithConfig(cfg)
	cp, ok := sp.(runtime.ConfirmingNudgeProvider)
	if !ok {
		t.Fatal("seam-backed tmux provider does not implement runtime.ConfirmingNudgeProvider; the strand can be neither reported nor recovered")
	}

	submitted, err := cp.NudgeConfirm(sessionName, runtime.TextContent("hello-seam-recover"))
	if err != nil {
		t.Fatalf("NudgeConfirm: %v", err)
	}
	out, captureErr := tm.CapturePaneAll(sessionName)
	if captureErr != nil {
		t.Fatalf("CapturePaneAll: %v", captureErr)
	}
	if !submitted {
		t.Fatalf("NudgeConfirm reported submitted=false for a pane that accepts a later Enter; the strand was detected and then dropped instead of retried (ga-8tno):\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("ENTER#%d", submitEnterMaxSends+1)) {
		t.Fatalf("no Enter was sent beyond the first attempt's budget, so no retry happened:\n%s", out)
	}
	if !strings.Contains(out, "esc to interrupt") {
		t.Fatalf("pane never reached submitted/busy state after the retry:\n%s", out)
	}
}
