package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
)

func TestNudgeUnconfirmedCheckOKWhenNoDiagnostics(t *testing.T) {
	cityRoot := t.TempDir()
	c := NewNudgeUnconfirmedCheck()
	r := c.Run(&CheckContext{CityPath: cityRoot})
	if r.Status != StatusOK {
		t.Fatalf("expected StatusOK, got %v: %s", r.Status, r.Message)
	}
}

func TestNudgeUnconfirmedCheckErrorsWhenDiagnosticsCannotBeRead(t *testing.T) {
	cityRoot := t.TempDir()
	sessionsDir := citylayout.SessionDiagnosticsDir(cityRoot)
	if err := os.MkdirAll(filepath.Dir(sessionsDir), 0o755); err != nil {
		t.Fatalf("mkdir runtime root: %v", err)
	}
	if err := os.WriteFile(sessionsDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write sessions path as file: %v", err)
	}

	r := NewNudgeUnconfirmedCheck().Run(&CheckContext{CityPath: cityRoot})
	if r.Status != StatusError || r.Severity != SeverityAdvisory {
		t.Fatalf("unreadable diagnostics status = %v severity = %v, want advisory error: %s", r.Status, r.Severity, r.Message)
	}
	if !strings.Contains(r.Message, "cannot read session diagnostic directory") {
		t.Fatalf("unreadable diagnostics message = %q, want read failure", r.Message)
	}
}

func TestNudgeUnconfirmedCheckWarnsOnDiagnosticFile(t *testing.T) {
	cityRoot := t.TempDir()
	sessionDir := filepath.Join(citylayout.SessionDiagnosticsDir(cityRoot), "gc-worker-1")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "nudge-unconfirmed.log"), []byte("unconfirmed"), 0o644); err != nil {
		t.Fatalf("write diagnostic: %v", err)
	}

	c := NewNudgeUnconfirmedCheck()
	r := c.Run(&CheckContext{CityPath: cityRoot})
	if r.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v: %s", r.Status, r.Message)
	}
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], "gc-worker-1") || !strings.Contains(r.Details[0], "nudge-unconfirmed.log") {
		t.Fatalf("expected details to name session and file, got %v", r.Details)
	}
}

func TestNudgeUnconfirmedCheckWarnsOnStartupDiagnosticFile(t *testing.T) {
	cityRoot := t.TempDir()
	sessionDir := filepath.Join(citylayout.SessionDiagnosticsDir(cityRoot), "gc-worker-2")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "startup-nudge-unconfirmed.log"), []byte("unconfirmed"), 0o644); err != nil {
		t.Fatalf("write diagnostic: %v", err)
	}

	c := NewNudgeUnconfirmedCheck()
	r := c.Run(&CheckContext{CityPath: cityRoot})
	if r.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v: %s", r.Status, r.Message)
	}
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], "startup-nudge-unconfirmed.log") {
		t.Fatalf("expected details to name startup diagnostic file, got %v", r.Details)
	}
}

// TestNudgeUnconfirmedCheckCountsSessionsNotArtifacts: a session holding both
// diagnostic files is one session with two artifacts. Counting details made
// the message report it as two sessions.
func TestNudgeUnconfirmedCheckCountsSessionsNotArtifacts(t *testing.T) {
	cityRoot := t.TempDir()
	sessionDir := filepath.Join(citylayout.SessionDiagnosticsDir(cityRoot), "gc-worker-3")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, filename := range []string{"nudge-unconfirmed.log", "startup-nudge-unconfirmed.log"} {
		if err := os.WriteFile(filepath.Join(sessionDir, filename), []byte("unconfirmed"), 0o644); err != nil {
			t.Fatalf("write %s: %v", filename, err)
		}
	}

	r := NewNudgeUnconfirmedCheck().Run(&CheckContext{CityPath: cityRoot})
	if r.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v: %s", r.Status, r.Message)
	}
	if len(r.Details) != 2 {
		t.Fatalf("expected one detail per artifact, got %v", r.Details)
	}
	if !strings.Contains(r.Message, "1 session(s)") {
		t.Fatalf("message = %q, want it to name 1 session for one session holding two artifacts", r.Message)
	}
}

func TestNudgeUnconfirmedCheckReportsNoBusyIndicatorAsInformational(t *testing.T) {
	cityRoot := t.TempDir()
	session := "gc-provider-without-busy-indicator"
	path := filepath.Join(citylayout.SessionDiagnosticsDir(cityRoot), session, "nudge-unconfirmed.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "session: " + session + "\ncause: submit delivered but not confirmed (no busy-state indicator for this provider family)\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write diagnostic: %v", err)
	}

	r := NewNudgeUnconfirmedCheck().Run(&CheckContext{CityPath: cityRoot})
	if r.Status != StatusOK {
		t.Fatalf("no-busy-indicator diagnostic warned: status = %v: %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "informational") || !strings.Contains(r.Message, "no busy-state indicator") {
		t.Fatalf("message = %q, want a separate informational count for providers without a busy-state indicator", r.Message)
	}
	if strings.Contains(r.Message, "1 session(s) have an unconfirmed nudge delivery") {
		t.Fatalf("no-busy-indicator diagnostic was counted as a delivery warning: %q", r.Message)
	}
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], session) {
		t.Fatalf("details = %v, want the informational artifact named", r.Details)
	}
}

func TestNudgeUnconfirmedCheckSeparatesInformationalFromWarnings(t *testing.T) {
	cityRoot := t.TempDir()
	writeNudgeDiagnostic(t, cityRoot, "gc-confirmable-provider", "nudge-unconfirmed.log")
	path := filepath.Join(citylayout.SessionDiagnosticsDir(cityRoot), "gc-provider-without-busy-indicator", "nudge-unconfirmed.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "session: gc-provider-without-busy-indicator\ncause: submit delivered but not confirmed (no busy-state indicator for this provider family)\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write no-busy-indicator diagnostic: %v", err)
	}

	r := NewNudgeUnconfirmedCheck().Run(&CheckContext{CityPath: cityRoot})
	if r.Status != StatusWarning {
		t.Fatalf("confirmable-provider diagnostic was hidden: status = %v: %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "1 session(s) have an unconfirmed nudge delivery") {
		t.Fatalf("message = %q, want only the confirmable provider in the warning count", r.Message)
	}
	if !strings.Contains(r.Message, "1 session(s) have unconfirmable deliveries (informational") {
		t.Fatalf("message = %q, want the no-busy-indicator delivery reported separately", r.Message)
	}
	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, "gc-confirmable-provider") || !strings.Contains(joined, "gc-provider-without-busy-indicator") {
		t.Fatalf("details = %v, want both the warning and informational sessions", r.Details)
	}
}

func TestNudgeUnconfirmedCheckIgnoresOldDiagnostics(t *testing.T) {
	cityRoot := t.TempDir()
	writeNudgeDiagnostic(t, cityRoot, "gc-old-submit", "nudge-unconfirmed.log")
	writeNudgeDiagnostic(t, cityRoot, "gc-old-startup", "startup-nudge-unconfirmed.log")
	writeNudgeDiagnostic(t, cityRoot, "gc-recent", "nudge-unconfirmed.log")

	stale := time.Now().Add(-48 * time.Hour)
	for _, path := range []string{
		filepath.Join(citylayout.SessionDiagnosticsDir(cityRoot), "gc-old-submit", "nudge-unconfirmed.log"),
		filepath.Join(citylayout.SessionDiagnosticsDir(cityRoot), "gc-old-startup", "startup-nudge-unconfirmed.log"),
	} {
		if err := os.Chtimes(path, stale, stale); err != nil {
			t.Fatalf("age diagnostic %s: %v", path, err)
		}
	}

	r := NewNudgeUnconfirmedCheck().Run(&CheckContext{CityPath: cityRoot})
	if r.Status != StatusWarning {
		t.Fatalf("recent diagnostic was hidden by old artifacts: status = %v: %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "1 session(s)") {
		t.Fatalf("message = %q, want only the recent session counted", r.Message)
	}
	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, "gc-recent") {
		t.Fatalf("details = %v, want the recent diagnostic", r.Details)
	}
	if strings.Contains(joined, "gc-old-submit") || strings.Contains(joined, "gc-old-startup") {
		t.Fatalf("stale diagnostics were reported: %v", r.Details)
	}
}

// TestNudgeUnconfirmedCheckReportsNonDirectoryEntries: a stray file directly
// under the sessions dir is malformed state, not clean state. It must be
// reported without suppressing the real diagnostics found alongside it.
func TestNudgeUnconfirmedCheckReportsNonDirectoryEntries(t *testing.T) {
	cityRoot := t.TempDir()
	sessionsDir := citylayout.SessionDiagnosticsDir(cityRoot)
	sessionDir := filepath.Join(sessionsDir, "gc-worker-4")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "nudge-unconfirmed.log"), []byte("unconfirmed"), 0o644); err != nil {
		t.Fatalf("write diagnostic: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, "stray.txt"), []byte("not a session dir"), 0o644); err != nil {
		t.Fatalf("write stray entry: %v", err)
	}

	r := NewNudgeUnconfirmedCheck().Run(&CheckContext{CityPath: cityRoot})
	if r.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v: %s", r.Status, r.Message)
	}
	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, "gc-worker-4") {
		t.Fatalf("a stray entry suppressed the real diagnostic; details = %v", r.Details)
	}
	if !strings.Contains(joined, "stray.txt") {
		t.Fatalf("expected details to name the non-directory entry, got %v", r.Details)
	}
}

// TestNudgeUnconfirmedCheckDoesNotReportCleanOnMalformedOnly: malformed state
// with no valid session dirs must not read as clean.
func TestNudgeUnconfirmedCheckDoesNotReportCleanOnMalformedOnly(t *testing.T) {
	cityRoot := t.TempDir()
	sessionsDir := citylayout.SessionDiagnosticsDir(cityRoot)
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, "stray.txt"), []byte("not a session dir"), 0o644); err != nil {
		t.Fatalf("write stray entry: %v", err)
	}

	r := NewNudgeUnconfirmedCheck().Run(&CheckContext{CityPath: cityRoot})
	if r.Status == StatusOK {
		t.Fatalf("malformed sessions dir reported clean: %s", r.Message)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "stray.txt") {
		t.Fatalf("expected details to name the non-directory entry, got %v", r.Details)
	}
}

func TestNudgeUnconfirmedCheckCanFixIsFalse(t *testing.T) {
	c := NewNudgeUnconfirmedCheck()
	if c.CanFix() {
		t.Fatal("expected CanFix to be false")
	}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Fatalf("expected Fix to be a no-op, got %v", err)
	}
}

// writeNudgeDiagnostic creates <sessions dir>/<session>/<filename> under
// cityRoot.
func writeNudgeDiagnostic(t *testing.T, cityRoot, session, filename string) {
	t.Helper()
	dir := filepath.Join(citylayout.SessionDiagnosticsDir(cityRoot), session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), []byte("unconfirmed"), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
}

func liveSessions(names ...string) LiveSessionNamesFunc {
	return func() (map[string]struct{}, error) {
		live := make(map[string]struct{}, len(names))
		for _, n := range names {
			live[n] = struct{}{}
		}
		return live, nil
	}
}

// TestNudgeUnconfirmedCheckIgnoresClosedSessions: a diagnostic left behind by
// a session that has since closed can never be acted on and is never cleared,
// so counting it makes the warning grow with city age instead of with
// current delivery problems.
func TestNudgeUnconfirmedCheckIgnoresClosedSessions(t *testing.T) {
	cityRoot := t.TempDir()
	writeNudgeDiagnostic(t, cityRoot, "gc-closed-1", "startup-nudge-unconfirmed.log")
	writeNudgeDiagnostic(t, cityRoot, "gc-closed-2", "nudge-unconfirmed.log")

	r := NewNudgeUnconfirmedCheck(WithLiveSessionNames(liveSessions("gc-live-1"))).Run(&CheckContext{CityPath: cityRoot})
	if r.Status != StatusOK {
		t.Fatalf("closed-session diagnostics warned: status = %v: %s (details %v)", r.Status, r.Message, r.Details)
	}
	if !strings.Contains(r.Message, "2 closed session(s)") {
		t.Fatalf("message = %q, want it to say how many closed sessions were skipped", r.Message)
	}
}

// TestNudgeUnconfirmedCheckCountsOnlyLiveSessions: with live and closed
// sessions side by side, only the live one is reported.
func TestNudgeUnconfirmedCheckCountsOnlyLiveSessions(t *testing.T) {
	cityRoot := t.TempDir()
	writeNudgeDiagnostic(t, cityRoot, "gc-live-1", "startup-nudge-unconfirmed.log")
	writeNudgeDiagnostic(t, cityRoot, "gc-closed-1", "startup-nudge-unconfirmed.log")
	writeNudgeDiagnostic(t, cityRoot, "gc-closed-2", "nudge-unconfirmed.log")

	r := NewNudgeUnconfirmedCheck(WithLiveSessionNames(liveSessions("gc-live-1", "gc-idle-2"))).Run(&CheckContext{CityPath: cityRoot})
	if r.Status != StatusWarning {
		t.Fatalf("live-session diagnostic not reported: status = %v: %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "1 session(s)") {
		t.Fatalf("message = %q, want it to count only the live session", r.Message)
	}
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], "gc-live-1") {
		t.Fatalf("details = %v, want only the live session's artifact", r.Details)
	}
	if !strings.Contains(r.Message, "2 closed session(s)") {
		t.Fatalf("message = %q, want it to say how many closed sessions were skipped", r.Message)
	}
}

// TestNudgeUnconfirmedCheckCountsEverySessionWhenLiveSetUnknown: if the
// session store cannot be read the check cannot tell live from closed. It
// must keep reporting every diagnostic -- the pre-filter behavior -- and say
// why, rather than treat an unreadable store as "no live sessions" and go
// green.
func TestNudgeUnconfirmedCheckCountsEverySessionWhenLiveSetUnknown(t *testing.T) {
	cityRoot := t.TempDir()
	writeNudgeDiagnostic(t, cityRoot, "gc-a", "nudge-unconfirmed.log")
	writeNudgeDiagnostic(t, cityRoot, "gc-b", "startup-nudge-unconfirmed.log")

	unreadable := func() (map[string]struct{}, error) { return nil, errors.New("store offline") }
	r := NewNudgeUnconfirmedCheck(WithLiveSessionNames(unreadable)).Run(&CheckContext{CityPath: cityRoot})
	if r.Status != StatusWarning {
		t.Fatalf("unreadable live set hid diagnostics: status = %v: %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "2 session(s)") {
		t.Fatalf("message = %q, want both sessions counted", r.Message)
	}
	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, "store offline") {
		t.Fatalf("details = %v, want the reason the live filter was skipped", r.Details)
	}
}

// TestNudgeUnconfirmedCheckMalformedEntriesSurviveLiveFilter: a stray file is
// malformed state regardless of which sessions are live.
func TestNudgeUnconfirmedCheckMalformedEntriesSurviveLiveFilter(t *testing.T) {
	cityRoot := t.TempDir()
	writeNudgeDiagnostic(t, cityRoot, "gc-closed-1", "nudge-unconfirmed.log")
	if err := os.WriteFile(filepath.Join(citylayout.SessionDiagnosticsDir(cityRoot), "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write stray entry: %v", err)
	}

	r := NewNudgeUnconfirmedCheck(WithLiveSessionNames(liveSessions())).Run(&CheckContext{CityPath: cityRoot})
	if r.Status != StatusWarning {
		t.Fatalf("malformed entry hidden by live filter: status = %v: %s", r.Status, r.Message)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "stray.txt") {
		t.Fatalf("details = %v, want the stray entry named", r.Details)
	}
}
