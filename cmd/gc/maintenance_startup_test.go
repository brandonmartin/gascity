package main

import (
	"strings"
	"testing"
	"time"
)

// TestMaintenanceStartupLine verifies the always-on startup banner reports
// the interval and the per-stage wiring state (gc / snapshot), so operators
// can confirm from the supervisor log whether a scheduled run will actually
// reclaim disk. (gascity ga-tp7, ga-ule)
func TestMaintenanceStartupLine(t *testing.T) {
	t.Run("gc-wired-snapshot-observe-only", func(t *testing.T) {
		got := maintenanceStartupLine(168*time.Hour, true, false)
		for _, want := range []string{"store-maintenance: loop started", "interval=168h", "gc=wired", "snapshot=observe-only"} {
			if !strings.Contains(got, want) {
				t.Errorf("startup line missing %q\ngot: %q", want, got)
			}
		}
	})

	t.Run("both-wired", func(t *testing.T) {
		got := maintenanceStartupLine(24*time.Hour, true, true)
		for _, want := range []string{"interval=24h", "gc=wired", "snapshot=wired"} {
			if !strings.Contains(got, want) {
				t.Errorf("startup line missing %q\ngot: %q", want, got)
			}
		}
		if strings.Contains(got, "observe-only") {
			t.Errorf("fully-wired line must not claim observe-only; got: %q", got)
		}
	})

	t.Run("both-observe-only", func(t *testing.T) {
		got := maintenanceStartupLine(24*time.Hour, false, false)
		for _, want := range []string{"gc=observe-only", "snapshot=observe-only"} {
			if !strings.Contains(got, want) {
				t.Errorf("startup line missing %q\ngot: %q", want, got)
			}
		}
	})
}
