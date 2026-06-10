package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/api"
)

// newMaintenanceCmd constructs the `gc maintenance` parent command. Two
// subcommands land with bead ga-zn8: `status` (read-path) and `dolt-gc`
// (mutation). Both route through the supervisor API exclusively — there
// is no local fallback because the scheduler and its in-memory ring
// buffer live inside the supervisor process (no status files, per
// AGENTS.md). When the controller is down the commands exit with code 2.
func newMaintenanceCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "maintenance",
		Short: "Dolt store maintenance (gc + snapshot)",
		Long: `Manage periodic Dolt store maintenance (see docs/adr/0002-dolt-store-maintenance-runbook.md).

The weekly loop runs inside the supervisor process when [maintenance.dolt] enabled=true
in city.toml. 'status' shows loop state and recent runs; 'dolt-gc' triggers a manual run.`,
	}
	cmd.AddCommand(newMaintenanceStatusCmd(stdout, stderr))
	cmd.AddCommand(newMaintenanceDoltGCCmd(stdout, stderr))
	return cmd
}

func newMaintenanceStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Show Dolt store maintenance status",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return exitForCode(cmdMaintenanceStatus(jsonOut, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	return cmd
}

func newMaintenanceDoltGCCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		wait    bool
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:          "dolt-gc",
		Short:        "Trigger a Dolt store maintenance run",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return exitForCode(cmdMaintenanceDoltGC(wait, jsonOut, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "block until the run completes (exit 1 on failure)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	return cmd
}

func cmdMaintenanceStatus(jsonOut bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc maintenance status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 2
	}
	c, reason := maintenanceAPIClient(cityPath)
	return routeMaintenanceStatus(c, reason, jsonOut, stdout, stderr)
}

func cmdMaintenanceDoltGC(wait, jsonOut bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc maintenance dolt-gc: %v\n", err) //nolint:errcheck // best-effort stderr
		return 2
	}
	c, reason := maintenanceAPIClient(cityPath)
	return routeMaintenanceDoltGC(c, reason, wait, jsonOut, stdout, stderr)
}

// maintenanceAPIClient resolves the supervisor API client for the
// maintenance subcommands, or returns (nil, reason) when routing isn't
// available. Indirected through a var so tests inject a client pointed at
// httptest.Server or force a specific fallback reason without spinning up
// a real controller.
var maintenanceAPIClient = func(cityPath string) (*api.Client, string) {
	if c := apiClient(cityPath); c != nil {
		return c, ""
	}
	// Maintenance has no local fallback. A supervisor-managed city omits a
	// standalone [api] port (the supervisor serves the API on its own port via
	// city-scoped routes), so apiClient returns nil even though the controller
	// socket is alive; route to the supervisor-managed client directly rather
	// than reporting controller-down. General commands keep apiClient's
	// nil→local fallback, so this routing is scoped to maintenance. (gascity ga-tp7)
	//
	// Honor the GC_NO_API escape hatch: apiClient returns nil under it, and the
	// alive-hook/supervisor client below never re-check it, so without this guard
	// an explicit operator opt-out would be silently bypassed for maintenance.
	if disabled, _ := classifyGCNoAPI(os.Getenv("GC_NO_API")); !disabled {
		if apiRouteControllerAliveHook(cityPath) != 0 {
			if c := apiRouteSupervisorClientHook(cityPath); c != nil {
				return c, ""
			}
		}
	}
	return nil, apiClientFallbackReason(cityPath)
}

// routeMaintenanceStatus dispatches `gc maintenance status` to the
// supervisor API. There is no local fallback (the in-memory ring buffer
// lives in the supervisor), so a nil client exits with code 2 and a
// route=fallback reason=<code> log line documents the reason.
func routeMaintenanceStatus(c *api.Client, nilReason string, jsonOut bool, stdout, stderr io.Writer) int {
	const cmdName = "maintenance status"
	if c == nil {
		logRoute(stderr, cmdName, "fallback", nilReason)
		fmt.Fprintf(stderr, "gc maintenance status: supervisor not running (%s)\n", nilReason) //nolint:errcheck // best-effort stderr
		return 2
	}
	cr, err := c.GetMaintenanceStatus()
	if err == nil {
		logRoute(stderr, cmdName, "api", "")
		return renderMaintenanceStatus(cr, jsonOut, stdout)
	}
	if api.IsMaintenanceDisabled(err) {
		logRoute(stderr, cmdName, "api", "error")
		fmt.Fprintln(stderr, "gc maintenance status: "+err.Error()) //nolint:errcheck // best-effort stderr
		return 2
	}
	if !api.ShouldFallbackForRead(c, err) {
		logRoute(stderr, cmdName, "api", "error")
		fmt.Fprintf(stderr, "gc maintenance status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	logRoute(stderr, cmdName, "fallback", api.FallbackReason(c, err))
	fmt.Fprintf(stderr, "gc maintenance status: supervisor unavailable (%s)\n", api.FallbackReason(c, err)) //nolint:errcheck // best-effort stderr
	return 2
}

// routeMaintenanceDoltGC dispatches `gc maintenance dolt-gc` to the
// supervisor API. Exit codes match the bead spec: 0 on success/accepted,
// 1 on --wait failure, 2 when the supervisor is unreachable, 3 on 409
// (run already in progress).
func routeMaintenanceDoltGC(c *api.Client, nilReason string, wait, jsonOut bool, stdout, stderr io.Writer) int {
	const cmdName = "maintenance dolt-gc"
	if c == nil {
		logRoute(stderr, cmdName, "fallback", nilReason)
		fmt.Fprintf(stderr, "gc maintenance dolt-gc: supervisor not running (%s)\n", nilReason) //nolint:errcheck // best-effort stderr
		return 2
	}
	view, err := c.TriggerMaintenanceDoltGC(wait)
	if err == nil {
		logRoute(stderr, cmdName, "api", "")
		return renderMaintenanceTrigger(view, wait, jsonOut, stdout)
	}
	if api.IsMaintenanceInProgress(err) {
		logRoute(stderr, cmdName, "api", "in-progress")
		fmt.Fprintf(stderr, "gc maintenance dolt-gc: %v\n", err) //nolint:errcheck // best-effort stderr
		return 3
	}
	if api.IsMaintenanceDisabled(err) {
		logRoute(stderr, cmdName, "api", "error")
		fmt.Fprintln(stderr, "gc maintenance dolt-gc: "+err.Error()) //nolint:errcheck // best-effort stderr
		return 2
	}
	if !api.ShouldFallback(c, err) {
		logRoute(stderr, cmdName, "api", "error")
		fmt.Fprintf(stderr, "gc maintenance dolt-gc: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	logRoute(stderr, cmdName, "fallback", api.FallbackReason(c, err))
	fmt.Fprintf(stderr, "gc maintenance dolt-gc: supervisor unavailable (%s)\n", api.FallbackReason(c, err)) //nolint:errcheck // best-effort stderr
	return 2
}

// renderMaintenanceStatus prints the status view as human-readable text or
// JSON, appending a stale-read banner when the API cache age exceeds the
// shared threshold. Always returns 0 — status is purely informational.
func renderMaintenanceStatus(cr api.CachedRead[api.MaintenanceStatusView], jsonOut bool, stdout io.Writer) int {
	if jsonOut {
		// writeCLIJSONLine adds the ok:true success discriminator and emits
		// one compact line, matching the rest of the gc --json surface.
		_ = writeCLIJSONLine(stdout, map[string]any{
			"schema_version": maintenanceJSONSchemaVersion,
			"status":         cr.Body,
			"_cache_age_s":   cr.AgeSeconds,
		})
		return 0
	}
	v := cr.Body
	enabled := "no"
	if v.Enabled {
		enabled = "yes"
	}
	fmt.Fprintf(stdout, "Maintenance: enabled=%s interval=%s\n", enabled, formatDurationSeconds(v.IntervalSec)) //nolint:errcheck
	if v.InFlight {
		fmt.Fprintf(stdout, "In-flight run started at %s\n", v.InFlightStart) //nolint:errcheck
	}
	if v.LastRun != nil {
		fmt.Fprintf(stdout, "Last run: stage=%s at %s (%.1fs)\n", v.LastRun.Stage, v.LastRun.StartedAt, v.LastRun.DurationSeconds) //nolint:errcheck
		if line := maintenanceStoreSizeLine(v.LastRun.BeforeBytes, v.LastRun.AfterBytes); line != "" {
			fmt.Fprintln(stdout, line) //nolint:errcheck
		}
		if v.LastRun.Err != "" {
			fmt.Fprintf(stdout, "  error: %s\n", v.LastRun.Err) //nolint:errcheck
		}
	} else {
		fmt.Fprintln(stdout, "Last run: none") //nolint:errcheck
	}
	if v.NextScheduled != "" {
		fmt.Fprintf(stdout, "Next scheduled: %s\n", v.NextScheduled) //nolint:errcheck
	}
	if len(v.History) > 0 {
		fmt.Fprintf(stdout, "History (%d):\n", len(v.History)) //nolint:errcheck
		for _, r := range v.History {
			line := fmt.Sprintf("  %s  stage=%s  duration=%.1fs", r.StartedAt, r.Stage, r.DurationSeconds)
			if r.BeforeBytes != 0 || r.AfterBytes != 0 {
				line += "  reclaimed=" + formatMaintenanceBytes(r.BeforeBytes-r.AfterBytes)
			}
			if r.Err != "" {
				line += "  err=" + truncateMaintenance(r.Err, 80)
			}
			fmt.Fprintln(stdout, line) //nolint:errcheck
		}
	}
	if cr.AgeSeconds > cacheAgeBannerThresholdSeconds {
		fmt.Fprintf(stdout, "(cache age: %.0fs — reconciler may be lagging)\n", cr.AgeSeconds) //nolint:errcheck
	}
	return 0
}

// renderMaintenanceTrigger prints the trigger outcome and decides the exit
// code. Sync (--wait) returns 1 when the Run's stage names a failing
// phase (anything other than "done"); async always returns 0 when the
// supervisor accepted the request.
func renderMaintenanceTrigger(view api.MaintenanceTriggerView, wait, jsonOut bool, stdout io.Writer) int {
	if jsonOut {
		_ = writeCLIJSONLine(stdout, map[string]any{
			"schema_version": maintenanceJSONSchemaVersion,
			"trigger":        view,
		})
	} else {
		if view.Run != nil {
			fmt.Fprintf(stdout, "Maintenance run: stage=%s started=%s duration=%.1fs\n", view.Run.Stage, view.Run.StartedAt, view.Run.DurationSeconds) //nolint:errcheck
			if line := maintenanceStoreSizeLine(view.Run.BeforeBytes, view.Run.AfterBytes); line != "" {
				fmt.Fprintln(stdout, line) //nolint:errcheck
			}
			if view.Run.Err != "" {
				fmt.Fprintf(stdout, "  error: %s\n", view.Run.Err) //nolint:errcheck
			}
			if view.Run.SnapshotPath != "" {
				fmt.Fprintf(stdout, "  snapshot: %s\n", view.Run.SnapshotPath) //nolint:errcheck
			}
		} else {
			fmt.Fprintf(stdout, "Maintenance accepted: started_at=%s\n", view.StartedAt) //nolint:errcheck
		}
	}
	if wait && view.Run != nil && view.Run.Stage != "done" {
		return 1
	}
	return 0
}

func formatDurationSeconds(sec int64) string {
	if sec <= 0 {
		return "-"
	}
	d := time.Duration(sec) * time.Second
	return d.String()
}

func truncateMaintenance(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit-1] + "…"
}

// maintenanceJSONSchemaVersion is the schema_version emitted on the
// maintenance commands' --json success envelopes. Bump only on a breaking
// change to the result shape declared in
// schemas/maintenance/*/result.schema.json.
const maintenanceJSONSchemaVersion = "1"

// maintenanceStoreSizeLine renders the indented before/after/reclaimed store
// size summary for a run, or "" when neither size was measured (both zero).
// Surfacing reclaimed bytes lets an operator spot a zero-reclaim run at a
// glance instead of trusting a bare stage=done.
func maintenanceStoreSizeLine(before, after int64) string {
	if before == 0 && after == 0 {
		return ""
	}
	return fmt.Sprintf("  store: before=%s after=%s reclaimed=%s",
		formatMaintenanceBytes(before), formatMaintenanceBytes(after), formatMaintenanceBytes(before-after))
}

// formatMaintenanceBytes renders a byte count in binary units (B/KiB/MiB/…)
// to one decimal place, preserving sign so a store that grew during a run
// shows a negative reclaimed value.
func formatMaintenanceBytes(n int64) string {
	const unit = 1024
	sign := ""
	v := n
	if v < 0 {
		sign = "-"
		v = -v
	}
	if v < unit {
		return fmt.Sprintf("%s%dB", sign, v)
	}
	div, exp := int64(unit), 0
	for rest := v / unit; rest >= unit; rest /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%s%.1f%ciB", sign, float64(v)/float64(div), "KMGTPE"[exp])
}
