package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/session"
)

const (
	wakeAttemptChurnName = "wake-attempt-churn"

	// wakeAttemptChurnWarnThreshold is the consecutive wake-failure count that
	// surfaces a warning. Three, not two: two in a row is a plausible transient
	// spawn flake. Three, not five: defaultMaxWakeAttempts already quarantines
	// at five, and the docbook/meltron incident flapped creating→asleep three
	// times with no error surfaced.
	wakeAttemptChurnWarnThreshold = 3

	wakeAttemptChurnInspectHintFmt = "Inspect with: gc session peek %s"
)

// wakeAttemptChurnCheck flags sessions whose wake_attempts metadata is
// climbing without an intervening state=active (or awake) window — the
// creating→asleep flap that otherwise stays silent until quarantine.
type wakeAttemptChurnCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
	now      func() time.Time
}

func newWakeAttemptChurnCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *wakeAttemptChurnCheck {
	return &wakeAttemptChurnCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *wakeAttemptChurnCheck) Name() string { return wakeAttemptChurnName }

// CanFix returns false: deciding whether to wake, reset, or leave a flapping
// session belongs to an operator (or a dedicated recovery order), not this check.
func (c *wakeAttemptChurnCheck) CanFix() bool { return false }

// Fix is a no-op. Detection only.
func (c *wakeAttemptChurnCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *wakeAttemptChurnCheck) nowUTC() time.Time {
	if c != nil && c.now != nil {
		return c.now().UTC()
	}
	return time.Now().UTC()
}

// Run classifies every open session bead's wake_attempts counter. A session
// that reached state=active or state=awake is not churn: the reconciler's
// clearWakeFailures path owns that window. Advisory on every path so a
// flapping session cannot fail gc doctor (startup-health-episodes remains
// the blocking quarantine signal at defaultMaxWakeAttempts).
func (c *wakeAttemptChurnCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	result := &doctor.CheckResult{Name: c.Name(), Severity: doctor.SeverityAdvisory}
	if c == nil || c.newStore == nil {
		result.Status = doctor.StatusOK
		result.Message = "no store factory; nothing to check"
		return result
	}
	store, err := c.newStore(c.cityPath)
	if err != nil {
		return advisorySkip(c.Name(),
			fmt.Sprintf("skipping wake-attempt churn scan: opening bead store: %v", err),
			"fix bead store access, then rerun gc doctor")
	}
	infos, err := cliSessionFrontDoor(store, c.cfg, c.cityPath).List("", "")
	if err != nil {
		return advisorySkip(c.Name(),
			fmt.Sprintf("skipping wake-attempt churn scan: listing sessions: %v", err),
			"fix bead store access, then rerun gc doctor")
	}

	now := c.nowUTC()
	worst := doctor.StatusOK
	var details []string
	var firstHint string
	for _, info := range infos {
		status, detail := classifyWakeAttemptChurn(info, now)
		if status == doctor.StatusOK {
			continue
		}
		if status > worst {
			worst = status
		}
		details = append(details, detail)
		if firstHint == "" {
			firstHint = wakeAttemptChurnDisplayName(info)
		}
	}
	sort.Strings(details)
	result.Status = worst
	result.Details = details
	if worst == doctor.StatusOK {
		result.Message = "no wake-attempt churn"
		return result
	}
	result.Message = fmt.Sprintf("%d session(s) accruing wake failures without an active window", len(details))
	result.FixHint = fmt.Sprintf(wakeAttemptChurnInspectHintFmt, firstHint)
	return result
}

func advisorySkip(name, message, hint string) *doctor.CheckResult {
	r := warnCheck(name, message, hint, nil)
	r.Severity = doctor.SeverityAdvisory
	return r
}

// classifyWakeAttemptChurn turns one session's persisted wake-failure counter
// into a doctor result. The raw WakeAttemptsMetadata string is the source of
// truth (mirroring recordWakeFailure / clearWakeFailures): the parsed
// WakeAttempts int zeroes on missing or invalid input and would hide the
// exact counter the reconciler accrues.
func classifyWakeAttemptChurn(info session.Info, now time.Time) (doctor.CheckStatus, string) {
	attempts, _ := strconv.Atoi(info.WakeAttemptsMetadata)
	if attempts < wakeAttemptChurnWarnThreshold {
		return doctor.StatusOK, ""
	}
	if wakeAttemptHasActiveWindow(info.State) {
		return doctor.StatusOK, ""
	}
	detail := formatWakeAttemptChurnDetail(info, attempts, now)
	if attempts >= defaultMaxWakeAttempts {
		return doctor.StatusError, detail
	}
	return doctor.StatusWarning, detail
}

func wakeAttemptHasActiveWindow(state session.State) bool {
	return state == session.StateActive || state == session.StateAwake
}

func wakeAttemptChurnDisplayName(info session.Info) string {
	if name := strings.TrimSpace(info.SessionName); name != "" {
		return name
	}
	if alias := strings.TrimSpace(info.Alias); alias != "" {
		return alias
	}
	return strings.TrimSpace(info.ID)
}

func formatWakeAttemptChurnDetail(info session.Info, attempts int, now time.Time) string {
	name := wakeAttemptChurnDisplayName(info)
	state := string(info.State)
	if state == "" {
		state = "unknown"
	}
	lastWoke, gap := formatWakeAttemptLastWoke(info.LastWokeAt, now)
	return fmt.Sprintf("%s: wake_attempts=%d state=%s last_woke_at=%s (gap %s)",
		name, attempts, state, lastWoke, gap)
}

func formatWakeAttemptLastWoke(raw string, now time.Time) (stamp, gap string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "never", "n/a"
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return raw, "unparseable"
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	return t.UTC().Format(time.RFC3339), formatWakeAttemptGap(d)
}

func formatWakeAttemptGap(d time.Duration) string {
	d = d.Round(time.Minute)
	if d < time.Minute {
		return "0s"
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.String()
}
