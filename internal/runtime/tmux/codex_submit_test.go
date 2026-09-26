package tmux

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// nudgeKeystrokesForFamily composes the keys a nudge actually puts into a pane,
// in NudgeSession's order: the optional pre-submit Escape (step 3) followed by
// the declared submit sequence (step 5). It exists in the test because the
// hazard is the COMPOSITION of two independent tables, which neither table can
// state on its own.
func nudgeKeystrokesForFamily(family string) []string {
	var keys []string
	if !providerEnvSkipsEscape(family) {
		keys = append(keys, "Escape")
	}
	return append(keys, nudgeSubmitKeySequenceForFamily(family)...)
}

// TestCodexNudgeNeverSendsEscape pins ga-biss: a nudge must not interrupt a
// running codex turn. Escape is "cancel the current turn" ("Conversation
// interrupted"), so the delivery keystrokes are a single Enter — the TUI's
// queued-follow-up submit. A second Escape would also be backtrack. The
// swallowed-paste newline from #4706 is handled by retrying Enter once when
// the composer still holds a draft, not by putting Escape in this sequence.
func TestCodexNudgeNeverSendsEscape(t *testing.T) {
	got := nudgeKeystrokesForFamily("codex")
	want := []string{"Enter"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("codex nudge keystrokes = %v, want %v", got, want)
	}
	if got := nudgeSubmitKeySequenceForFamily("codex"); !reflect.DeepEqual(got, want) {
		t.Fatalf("codex submit sequence = %v, want %v", got, want)
	}
	for _, key := range got {
		if key == "Escape" {
			t.Fatalf("codex nudge keystrokes include Escape: %v", got)
		}
	}
	if !providerEnvSkipsEscape("codex") {
		t.Fatal("codex left providersSkippingEscapeBeforeEnter; the pre-submit Escape would interrupt a running turn")
	}
}

// Control: the claude path is byte-identical to what it has always been. If the
// codex entry had been added to a shared default, or the skip list had been
// edited instead of the table, this row would move too.
func TestClaudeNudgeKeystrokesAreUnchanged(t *testing.T) {
	if got, want := nudgeKeystrokesForFamily("claude"), []string{"Enter"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("claude nudge keystrokes = %v, want %v", got, want)
	}
	// And an unregistered family keeps the historical default-with-Escape shape.
	if got, want := nudgeKeystrokesForFamily("some-unregistered-family"), []string{"Escape", "Enter"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unregistered family keystrokes = %v, want %v", got, want)
	}
}

// TestCodexSubmitIsVerified pins the second half of the codex fix: its submit is
// CONFIRMED against the busy indicator paneContainsBusyIndicator already reads
// for it ("esc to interrupt"). Without eligibility the best-effort fallback
// reports success the moment keys reach tmux, so the queue acks and deletes an
// item whose paste may still be unsubmitted — losing the nudge outright instead
// of requeueing it under the attempt cap.
func TestCodexSubmitIsVerified(t *testing.T) {
	for _, family := range []string{"claude", "codex"} {
		if !submitVerifyEligibleFamily(family) {
			t.Errorf("submitVerifyEligibleFamily(%q) = false, want true", family)
		}
	}
	// Control: a family whose busy indicator this package cannot read stays on
	// best-effort delivery, because reporting every submit as unconfirmed would
	// burn the queue's attempts re-pasting messages that already landed.
	for _, family := range []string{"grok", "kimi", "opencode", "", "some-unregistered-family"} {
		if submitVerifyEligibleFamily(family) {
			t.Errorf("submitVerifyEligibleFamily(%q) = true, want false", family)
		}
	}
}

// TestCodexBusyIndicatorIsReadable is the premise the eligibility above rests
// on: if codex's indicator string ever stops matching, verification would report
// every codex submit as unconfirmed and the queue would re-paste every nudge
// three times before dead-lettering it.
func TestCodexBusyIndicatorIsReadable(t *testing.T) {
	if !paneContainsBusyIndicator([]string{"  working  (esc to interrupt)"}) {
		t.Fatal("codex's busy indicator no longer matches; submit verification for codex would report every delivery unconfirmed")
	}
}

// TestNudgeSessionComposesEscapeThenSubmitSequence pins the PROVENANCE of the
// composition nudgeKeystrokesForFamily models.
//
// The two decisions live in NudgeSession's body — the pre-submit Escape at step
// 3, then the declared submit sequence at step 5 — and driving that body needs a
// live tmux pane (both decisions resolve through the pane's environment or a
// process sniff), so a unit test cannot execute it. What it CAN do is refuse to
// let the model drift from the code: if NudgeSession stops consulting either
// decision, or consults them in the other order, the helper above is describing
// something that no longer happens and this fails.
func TestNudgeSessionComposesEscapeThenSubmitSequence(t *testing.T) {
	body := nudgeSessionSource(t)
	escapeAt := strings.Index(body, "t.shouldSendEscapeBeforeEnter(target)")
	submitAt := strings.Index(body, "t.nudgeSubmitKeySequence(target)")
	switch {
	case escapeAt < 0:
		t.Fatal("NudgeSession no longer consults shouldSendEscapeBeforeEnter; nudgeKeystrokesForFamily models a step that is gone")
	case submitAt < 0:
		t.Fatal("NudgeSession no longer consults nudgeSubmitKeySequence; nudgeKeystrokesForFamily models a step that is gone")
	case submitAt < escapeAt:
		t.Fatal("NudgeSession now resolves the submit sequence BEFORE the pre-submit Escape decision; re-derive nudgeKeystrokesForFamily against the new order")
	}
}

func TestCodexComposerDraftIsTheBottomGlyphLine(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"empty composer", []string{"│ ›"}, false},
		{"empty composer with space", []string{"│ › "}, false},
		{"drafted nudge", []string{"│ › Run 'gc prime' to check worker status"}, true},
		{"ascii prompt draft", []string{"> Run gc prime"}, true},
		{
			"scrollback user line above an empty composer",
			[]string{"› earlier submitted turn", "│ ›"},
			false,
		},
		{"no composer line", []string{"hello from cat"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexComposerHasDraft(tc.lines); got != tc.want {
				t.Fatalf("codexComposerHasDraft() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCodexComposerLivenessFailureRequiresDraftAndNoActivity(t *testing.T) {
	draft := []string{"│ › Run 'gc prime' to check worker status"}
	if !codexComposerLivenessFailure(draft) {
		t.Fatal("draft with no busy indicator must be a liveness failure")
	}
	busy := append(append([]string{}, draft...), "◦ Working (2m 48s • esc to interrupt)")
	if codexComposerLivenessFailure(busy) {
		t.Fatal("a working turn is activity, even with text still in the composer")
	}
	if codexComposerLivenessFailure([]string{"│ ›"}) {
		t.Fatal("an empty composer is idle, not a liveness failure")
	}
}

func TestConfirmCodexFollowUpAcceptsQueuedFollowUpWithoutSecondEnter(t *testing.T) {
	var enters int
	// Already-busy pane whose composer clears on the first Enter: the nudge
	// queued behind the running turn. A second Enter would be a second message.
	err := confirmCodexFollowUp(
		func() error { enters++; return nil },
		func() (bool, bool, error) { return false, true, nil },
		func(time.Duration) {},
	)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if enters != 1 {
		t.Fatalf("enters = %d, want 1", enters)
	}
}

func TestConfirmCodexFollowUpRetriesEnterOnceWhenDraftRemains(t *testing.T) {
	var enters int
	err := confirmCodexFollowUp(
		func() error { enters++; return nil },
		func() (bool, bool, error) { return enters < 2, false, nil },
		func(time.Duration) {},
	)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if enters != 2 {
		t.Fatalf("enters = %d, want 2 (initial Enter plus one retry)", enters)
	}
}

func TestConfirmCodexFollowUpLivenessWhenDraftHasNoActivity(t *testing.T) {
	var enters int
	err := confirmCodexFollowUp(
		func() error { enters++; return nil },
		func() (bool, bool, error) { return true, false, nil },
		func(time.Duration) {},
	)
	if !errors.Is(err, ErrCodexComposerLiveness) {
		t.Fatalf("err = %v, want ErrCodexComposerLiveness", err)
	}
	if !errors.Is(err, ErrNudgeSubmitUnconfirmed) {
		t.Fatal("liveness failure must wrap ErrNudgeSubmitUnconfirmed so the queue retries")
	}
	if enters != 1+codexFollowUpEnterRetries {
		t.Fatalf("enters = %d, want %d", enters, 1+codexFollowUpEnterRetries)
	}
}

func TestConfirmCodexFollowUpUnconfirmedWhenBusyDraftRemains(t *testing.T) {
	err := confirmCodexFollowUp(
		func() error { return nil },
		func() (bool, bool, error) { return true, true, nil },
		func(time.Duration) {},
	)
	if errors.Is(err, ErrCodexComposerLiveness) {
		t.Fatal("a busy pane is activity; a remaining draft is an unconfirmed submit, not the no-activity liveness failure")
	}
	if !errors.Is(err, ErrNudgeSubmitUnconfirmed) {
		t.Fatalf("err = %v, want ErrNudgeSubmitUnconfirmed", err)
	}
}

// TestNudgeSessionConfirmsCodexByComposer pins the call order. The busy-indicator
// confirm false-succeeds when the turn was already running, which is exactly
// when a follow-up must queue instead of looking delivered.
func TestNudgeSessionConfirmsCodexByComposer(t *testing.T) {
	body := nudgeSessionSource(t)
	codexAt := strings.Index(body, "t.isCodexTarget(target)")
	confirmAt := strings.Index(body, "confirmCodexFollowUp(")
	busyAt := strings.Index(body, "t.submitVerifyEligible(target)")
	switch {
	case codexAt < 0 || confirmAt < 0:
		t.Fatal("NudgeSession no longer confirms codex through confirmCodexFollowUp")
	case busyAt < 0:
		t.Fatal("NudgeSession no longer consults submitVerifyEligible")
	case codexAt > busyAt || confirmAt > busyAt:
		t.Fatal("codex follow-up confirm must run before the busy-indicator confirm")
	}
}

// nudgeSessionSource returns the source text of NudgeSession's body.
func nudgeSessionSource(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("tmux.go")
	if err != nil {
		t.Fatalf("reading tmux.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func (t *Tmux) NudgeSession(")
	if start < 0 {
		t.Fatal("NudgeSession not found in tmux.go")
	}
	rest := body[start:]
	end := strings.Index(rest, "\nfunc ")
	if end < 0 {
		return rest
	}
	return rest[:end]
}
