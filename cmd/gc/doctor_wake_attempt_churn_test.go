package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/session"
)

func wakeAttemptSessionInfo(name, state, attempts, lastWokeAt string) session.Info {
	return session.Info{
		ID:                   name,
		SessionName:          name,
		State:                session.State(state),
		WakeAttempts:         mustAtoiOrZero(attempts),
		WakeAttemptsMetadata: attempts,
		LastWokeAt:           lastWokeAt,
	}
}

func mustAtoiOrZero(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func TestClassifyWakeAttemptChurnBelowThresholdIsOK(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	info := wakeAttemptSessionInfo("docbook/meltron", "creating", "2", now.Add(-time.Minute).Format(time.RFC3339))

	status, detail := classifyWakeAttemptChurn(info, now)

	if status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK under threshold", status)
	}
	if detail != "" {
		t.Fatalf("detail = %q, want empty for a healthy session", detail)
	}
}

func TestClassifyWakeAttemptChurnAtWarnThresholdWithoutActiveIsWarning(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	lastWoke := now.Add(-45 * time.Minute)
	info := wakeAttemptSessionInfo("docbook/meltron", "creating", "3", lastWoke.Format(time.RFC3339))

	status, detail := classifyWakeAttemptChurn(info, now)

	if status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning at 3 creating→asleep flaps", status)
	}
	for _, want := range []string{"docbook/meltron", "wake_attempts=3", "state=creating", lastWoke.Format(time.RFC3339), "45m"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail missing %q: %q", want, detail)
		}
	}
}

func TestClassifyWakeAttemptChurnAtErrorThresholdIsError(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	info := wakeAttemptSessionInfo("docbook/meltron", "asleep", "5", now.Add(-2*time.Hour).Format(time.RFC3339))

	status, detail := classifyWakeAttemptChurn(info, now)

	if status != doctor.StatusError {
		t.Fatalf("status = %v, want Error at quarantine threshold", status)
	}
	if !strings.Contains(detail, "wake_attempts=5") {
		t.Fatalf("detail = %q, want wake_attempts=5", detail)
	}
	if !strings.Contains(detail, "2h") {
		t.Fatalf("detail = %q, want last_woke_at gap 2h", detail)
	}
}

func TestClassifyWakeAttemptChurnSkipsActiveWindow(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	info := wakeAttemptSessionInfo("stable-worker", "active", "4", now.Add(-time.Minute).Format(time.RFC3339))

	status, detail := classifyWakeAttemptChurn(info, now)

	if status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK when state=active even with a high counter", status)
	}
	if detail != "" {
		t.Fatalf("detail = %q, want empty when an active window is present", detail)
	}
}

func TestClassifyWakeAttemptChurnSkipsAwakeWindow(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	info := wakeAttemptSessionInfo("stable-worker", "awake", "6", now.Format(time.RFC3339))

	status, _ := classifyWakeAttemptChurn(info, now)
	if status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK for state=awake", status)
	}
}

func TestClassifyWakeAttemptChurnMissingLastWokeAtRendersNever(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	info := wakeAttemptSessionInfo("docbook/meltron", "asleep", "3", "")

	_, detail := classifyWakeAttemptChurn(info, now)
	if !strings.Contains(detail, "last_woke_at=never") {
		t.Fatalf("detail = %q, want last_woke_at=never", detail)
	}
}

func TestClassifyWakeAttemptChurnParsesRawMetadataNotIntField(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	info := session.Info{
		ID:                   "raw-counter",
		SessionName:          "raw-counter",
		State:                session.StateCreating,
		WakeAttempts:         0, // int form zeroes on anything the codec cannot parse
		WakeAttemptsMetadata: "3",
	}

	status, detail := classifyWakeAttemptChurn(info, now)
	if status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning from raw wake_attempts metadata", status)
	}
	if !strings.Contains(detail, "wake_attempts=3") {
		t.Fatalf("detail = %q, want the raw counter, not the zeroed int field", detail)
	}
}

func seedWakeAttemptSession(t *testing.T, mem beads.Store, id, state, attempts, lastWokeAt string) {
	t.Helper()
	b := beads.Bead{
		ID:     id,
		Title:  id,
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":  id,
			"state":         state,
			"wake_attempts": attempts,
			"last_woke_at":  lastWokeAt,
		},
	}
	if _, err := mem.Create(b); err != nil {
		t.Fatalf("seeding session %q: %v", id, err)
	}
}

func TestWakeAttemptChurnCheckNoSessionsIsOK(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStore()

	result := newWakeAttemptChurnCheck(&config.City{}, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			t.Fatalf("unexpected store path %q", path)
		}
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok: %#v", result.Status, result)
	}
	if result.Severity != doctor.SeverityAdvisory {
		t.Errorf("Severity = %v, want Advisory (must not gate gc doctor)", result.Severity)
	}
	if result.Name != "wake-attempt-churn" {
		t.Errorf("Name = %q, want wake-attempt-churn", result.Name)
	}
}

func TestWakeAttemptChurnCheckWarnsOnCreatingFlap(t *testing.T) {
	cityDir := t.TempDir()
	mem := beads.NewMemStore()
	now := time.Date(2026, 9, 5, 15, 0, 0, 0, time.UTC)
	seedWakeAttemptSession(t, mem, "docbook/meltron", "creating", "3", now.Add(-10*time.Minute).Format(time.RFC3339))
	seedWakeAttemptSession(t, mem, "healthy", "active", "0", now.Format(time.RFC3339))

	check := newWakeAttemptChurnCheck(&config.City{}, cityDir, func(string) (beads.Store, error) {
		return mem, nil
	})
	check.now = func() time.Time { return now }

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning: %#v", result.Status, result)
	}
	if result.Severity != doctor.SeverityAdvisory {
		t.Errorf("Severity = %v, want Advisory", result.Severity)
	}
	if len(result.Details) != 1 {
		t.Fatalf("Details = %v, want only the flapping session", result.Details)
	}
	if !strings.Contains(result.Details[0], "docbook/meltron") {
		t.Errorf("Details missing flapping session: %v", result.Details)
	}
	if !strings.Contains(result.FixHint, "gc session peek docbook/meltron") {
		t.Errorf("FixHint = %q, want peek of the flapping session", result.FixHint)
	}
}

func TestWakeAttemptChurnCheckErrorOutranksWarning(t *testing.T) {
	cityDir := t.TempDir()
	mem := beads.NewMemStore()
	now := time.Date(2026, 9, 5, 15, 0, 0, 0, time.UTC)
	seedWakeAttemptSession(t, mem, "warn-session", "creating", "3", now.Format(time.RFC3339))
	seedWakeAttemptSession(t, mem, "error-session", "asleep", "5", now.Format(time.RFC3339))

	check := newWakeAttemptChurnCheck(&config.City{}, cityDir, func(string) (beads.Store, error) {
		return mem, nil
	})
	check.now = func() time.Time { return now }

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error when any session hits the higher threshold: %#v", result.Status, result)
	}
	if len(result.Details) != 2 {
		t.Fatalf("Details = %v, want both sessions", result.Details)
	}
}

func TestWakeAttemptChurnCheckStoreOpenErrorIsSkipWarning(t *testing.T) {
	result := newWakeAttemptChurnCheck(&config.City{}, t.TempDir(), func(string) (beads.Store, error) {
		return nil, errors.New("dolt circuit breaker is open")
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning on store open failure: %#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "skipping") {
		t.Errorf("Message = %q, want skip language", result.Message)
	}
}

func TestWakeAttemptChurnCheckCanFixFalse(t *testing.T) {
	check := newWakeAttemptChurnCheck(&config.City{}, t.TempDir(), nil)
	if check.CanFix() {
		t.Fatal("CanFix = true, want false")
	}
	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix = %v, want nil", err)
	}
	if check.WarmupEligible() {
		t.Fatal("WarmupEligible = true, want false")
	}
}
