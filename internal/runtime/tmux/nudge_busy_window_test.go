package tmux

import (
	"fmt"
	"testing"
)

// ga-61tq: the busy probe behind submit confirmation captures 120 lines of
// scrollback (`capture-pane -S -120`), so a *finished* turn's busy frame — long
// scrolled out of the live footer but still sitting in history — read as
// current activity.
//
// That false positive is what let a stranded nudge report a landed submit.
// submitEnterAndConfirm polls this signal after sending Enter: a stale frame
// answers "busy" on the first poll, so it returns confirmed=true even though
// the Enter never landed, and the re-send that exists precisely to recover a
// dropped Enter never fires. The reporting fix in ga-287 is downstream of this
// probe, so an honest delivery signal cannot survive a lying one.
//
// The witness saw the consequence as a spinner frozen at an identical elapsed
// value across peeks while the footer showed the idle form — history on screen,
// no live turn.
//
// Busy indicators are footer chrome. Only the newest rows a TUI paints describe
// what the agent is doing now.
func TestPaneShowsLiveBusyIndicatorIgnoresScrollbackHistory(t *testing.T) {
	lines := []string{
		"✻ Cooking… (24m 30s · ↓ 10.9k tokens)",
		"  esc to interrupt",
	}
	// Push the finished turn's frame out of the live footer region, the way
	// continued transcript output does in a real session.
	for i := 0; i < busyObservationLines*2; i++ {
		lines = append(lines, fmt.Sprintf("transcript line %d", i))
	}
	lines = append(lines,
		"❯ commit and run the done sequence",
		"  🧠 Opus 5 | 📁 slit | · 3 shells · ← for agents",
	)

	if paneShowsLiveBusyIndicator(lines) {
		t.Fatal("a finished turn's busy frame in scrollback read as live activity; a stranded draft will report as a landed submit")
	}
}

// The live signal must survive the bound, or every real turn reads idle and
// nudges land mid-turn.
func TestPaneShowsLiveBusyIndicatorDetectsLiveFooter(t *testing.T) {
	var lines []string
	for i := 0; i < busyObservationLines*3; i++ {
		lines = append(lines, fmt.Sprintf("transcript line %d", i))
	}
	lines = append(lines,
		"✻ Cooking… (2m 28s · ↓ 10.9k tokens)",
		"",
		"╭──────────────────────────────╮",
		"│ >                            │",
		"╰──────────────────────────────╯",
		"  🧠 Opus 5 | 📁 slit | esc to interrupt",
	)

	if !paneShowsLiveBusyIndicator(lines) {
		t.Fatal("live busy footer read as idle; nudges would land mid-turn")
	}
}

// tmux pads the captured grid with blank rows. Counting raw lines from the
// bottom would let that padding push the live footer out of the window, so the
// bound is measured in rows that actually carry content.
func TestPaneShowsLiveBusyIndicatorSurvivesBlankPadding(t *testing.T) {
	lines := []string{"✻ Cooking… (2m 28s · ↓ 10.9k tokens)", "  esc to interrupt"}
	for i := 0; i < busyObservationLines*3; i++ {
		lines = append(lines, "")
	}

	if !paneShowsLiveBusyIndicator(lines) {
		t.Fatal("blank grid padding pushed the live busy footer out of the observation window")
	}
}
