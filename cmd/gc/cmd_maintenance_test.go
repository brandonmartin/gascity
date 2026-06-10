package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

// okMaintenanceStatusHandler emits a plausible GET /maintenance/status
// body with one prior run and no in-flight cycle. Used as the
// api-happy-path fixture in the six-row matrix.
func okMaintenanceStatusHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-GC-Cache-Age-S", "2")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enabled":          true,
			"interval_seconds": int64(604800),
			"in_flight":        false,
			"last_run": map[string]any{
				"started_at":   "2026-04-22T03:00:00Z",
				"finished_at":  "2026-04-22T03:00:05Z",
				"stage":        "done",
				"before_bytes": int64(11000000000),
				"after_bytes":  int64(2000000000),
				"duration_s":   5.0,
			},
			"history": []map[string]any{
				{
					"started_at":   "2026-04-22T03:00:00Z",
					"finished_at":  "2026-04-22T03:00:05Z",
					"stage":        "done",
					"before_bytes": int64(11000000000),
					"after_bytes":  int64(2000000000),
					"duration_s":   5.0,
				},
			},
		})
	})
}

// okMaintenanceTriggerHandler emits a 202 response body carrying a
// synthetic started_at; the handler is idempotent so every request the
// test makes sees the same body regardless of order.
func okMaintenanceTriggerHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted":   true,
			"started_at": "2026-04-22T03:00:00Z",
		})
	})
}

// okMaintenanceTriggerWaitHandler emits the full 202 body for a synchronous
// (?wait=true) call. The run has stage=done so the CLI exits 0.
func okMaintenanceTriggerWaitHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted":   true,
			"started_at": "2026-04-22T03:00:00Z",
			"run": map[string]any{
				"started_at":   "2026-04-22T03:00:00Z",
				"finished_at":  "2026-04-22T03:00:05Z",
				"stage":        "done",
				"before_bytes": int64(11000000000),
				"after_bytes":  int64(2000000000),
				"duration_s":   5.0,
			},
		})
	})
}

// maintenanceProblemHandler returns a Huma-style Problem Details body at
// the configured status/detail. Matches the test scaffolding used by the
// mail and order read-path matrices so the assertion helpers stay shared.
func maintenanceProblemHandler(status int, detail string) func(*testing.T) http.Handler {
	return func(_ *testing.T) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": status,
				"title":  http.StatusText(status),
				"detail": detail,
			})
		})
	}
}

// assertMaintenanceRouteLog verifies exactly one route=... line with the
// expected shape is present in stderr. Empty wantRoute means "do not assert
// routing" — some rows assert an API error instead.
func assertMaintenanceRouteLog(t *testing.T, stderrStr, wantRoute, wantReason string) {
	t.Helper()
	if wantRoute == "" {
		return
	}
	want := "route=" + wantRoute
	if wantReason != "" {
		want += " reason=" + wantReason
	}
	if !strings.Contains(stderrStr, want) {
		t.Errorf("stderr missing %q:\n%s", want, stderrStr)
	}
	if n := strings.Count(stderrStr, "route="); n != 1 {
		t.Errorf("route=... lines = %d, want 1:\n%s", n, stderrStr)
	}
}

// TestRouteMaintenanceStatus_SixRowMatrix exercises the six mandatory
// rows for `gc maintenance status`. Exit codes diverge from the generic
// mail pattern because no local fallback exists for maintenance reads —
// the in-memory ring buffer lives only in the supervisor process.
// Fallback rows therefore exit 2 (supervisor unreachable) rather than 0.
func TestRouteMaintenanceStatus_SixRowMatrix(t *testing.T) {
	tests := []struct {
		name         string
		handler      func(*testing.T) http.Handler
		useNilClient bool
		nilReason    string
		wantExit     int
		wantRoute    string
		wantReason   string
		wantStderr   string
		wantStdout   string
	}{
		{
			name:       "api-happy-path",
			handler:    okMaintenanceStatusHandler,
			wantExit:   0,
			wantRoute:  "api",
			wantStdout: "Maintenance: enabled=yes",
		},
		{
			name:       "api-cache-not-live",
			handler:    maintenanceProblemHandler(http.StatusServiceUnavailable, "cache_not_live: supervisor cache is priming"),
			wantExit:   2, // no local fallback source
			wantRoute:  "fallback",
			wantReason: "cache-not-live",
		},
		{
			name:       "api-500-fallback",
			handler:    maintenanceProblemHandler(http.StatusInternalServerError, "internal: something exploded"),
			wantExit:   2,
			wantRoute:  "fallback",
			wantReason: "conn-refused",
		},
		{
			name:       "api-404-error",
			handler:    maintenanceProblemHandler(http.StatusNotFound, "not_found: no such thing"),
			wantExit:   1,
			wantStderr: "not_found",
		},
		{
			name:         "controller-down",
			useNilClient: true,
			nilReason:    "controller-down",
			wantExit:     2,
			wantRoute:    "fallback",
			wantReason:   "controller-down",
		},
		{
			name:         "escape-hatch",
			useNilClient: true,
			nilReason:    "escape-hatch",
			wantExit:     2,
			wantRoute:    "fallback",
			wantReason:   "escape-hatch",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_DEBUG", "1")

			var c *api.Client
			if !tc.useNilClient {
				srv := httptest.NewServer(tc.handler(t))
				defer srv.Close()
				c = api.NewCityScopedClient(srv.URL, "test-city")
			}

			var stdout, stderr bytes.Buffer
			code := routeMaintenanceStatus(c, tc.nilReason, false, &stdout, &stderr)
			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q", code, tc.wantExit, stderr.String(), stdout.String())
			}
			assertMaintenanceRouteLog(t, stderr.String(), tc.wantRoute, tc.wantReason)
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantStderr, stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout missing %q:\n%s", tc.wantStdout, stdout.String())
			}
		})
	}
}

// TestRouteMaintenanceDoltGC_SixRowMatrix exercises the six mandatory
// rows for `gc maintenance dolt-gc`. Like the status subcommand there is
// no local fallback; unlike status, this command is a mutator and uses
// ShouldFallback (not ShouldFallbackForRead) so generic 5xx is a hard
// error (exit 1), not a fallback.
func TestRouteMaintenanceDoltGC_SixRowMatrix(t *testing.T) {
	tests := []struct {
		name         string
		handler      func(*testing.T) http.Handler
		useNilClient bool
		nilReason    string
		wantExit     int
		wantRoute    string
		wantReason   string
		wantStderr   string
		wantStdout   string
	}{
		{
			name:       "api-happy-path",
			handler:    okMaintenanceTriggerHandler,
			wantExit:   0,
			wantRoute:  "api",
			wantStdout: "Maintenance accepted",
		},
		{
			name:       "api-cache-not-live",
			handler:    maintenanceProblemHandler(http.StatusServiceUnavailable, "cache_not_live: supervisor cache is priming"),
			wantExit:   2,
			wantRoute:  "fallback",
			wantReason: "cache-not-live",
		},
		{
			name:       "api-500-fallback",
			handler:    maintenanceProblemHandler(http.StatusInternalServerError, "internal: something exploded"),
			wantExit:   1, // mutation: generic 5xx is a hard error
			wantStderr: "API error",
		},
		{
			name:       "api-404-error",
			handler:    maintenanceProblemHandler(http.StatusNotFound, "not_found: no such thing"),
			wantExit:   1,
			wantStderr: "not_found",
		},
		{
			name:         "controller-down",
			useNilClient: true,
			nilReason:    "controller-down",
			wantExit:     2,
			wantRoute:    "fallback",
			wantReason:   "controller-down",
		},
		{
			name:         "escape-hatch",
			useNilClient: true,
			nilReason:    "escape-hatch",
			wantExit:     2,
			wantRoute:    "fallback",
			wantReason:   "escape-hatch",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_DEBUG", "1")

			var c *api.Client
			if !tc.useNilClient {
				srv := httptest.NewServer(tc.handler(t))
				defer srv.Close()
				c = api.NewCityScopedClient(srv.URL, "test-city")
			}

			var stdout, stderr bytes.Buffer
			code := routeMaintenanceDoltGC(c, tc.nilReason, false, false, &stdout, &stderr)
			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q", code, tc.wantExit, stderr.String(), stdout.String())
			}
			assertMaintenanceRouteLog(t, stderr.String(), tc.wantRoute, tc.wantReason)
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantStderr, stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout missing %q:\n%s", tc.wantStdout, stdout.String())
			}
		})
	}
}

// TestRouteMaintenanceDoltGC_InProgress verifies 409 with
// maintenance-in-progress body maps to exit 3 (bead's documented exit
// code for "already running").
func TestRouteMaintenanceDoltGC_InProgress(t *testing.T) {
	t.Setenv("GC_DEBUG", "1")
	body := `maintenance-in-progress: {"type":"maintenance-in-progress","started_at":"2026-04-22T03:00:00Z"}`
	srv := httptest.NewServer(maintenanceProblemHandler(http.StatusConflict, body)(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := routeMaintenanceDoltGC(c, "", false, false, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit = %d, want 3 (already running); stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "already in progress") {
		t.Errorf("stderr missing 'already in progress':\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "2026-04-22T03:00:00Z") {
		t.Errorf("stderr missing in-flight started_at:\n%s", stderr.String())
	}
}

// TestRouteMaintenanceDoltGC_WaitFailure verifies that --wait returning a
// run with stage!='done' maps to exit 1.
func TestRouteMaintenanceDoltGC_WaitFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted":   true,
			"started_at": "2026-04-22T03:00:00Z",
			"run": map[string]any{
				"started_at":  "2026-04-22T03:00:00Z",
				"finished_at": "2026-04-22T03:00:01Z",
				"stage":       "gc",
				"err":         "CALL DOLT_GC(): lock timeout",
				"duration_s":  1.0,
			},
		})
	}))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := routeMaintenanceDoltGC(c, "", true, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (wait + failure); stdout=%q", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "stage=gc") {
		t.Errorf("stdout missing stage=gc:\n%s", stdout.String())
	}
}

// TestRouteMaintenanceDoltGC_WaitSuccess verifies that --wait returning a
// run with stage='done' maps to exit 0.
func TestRouteMaintenanceDoltGC_WaitSuccess(t *testing.T) {
	srv := httptest.NewServer(okMaintenanceTriggerWaitHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := routeMaintenanceDoltGC(c, "", true, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "stage=done") {
		t.Errorf("stdout missing stage=done:\n%s", stdout.String())
	}
}

// TestMaintenanceCommandsDeclareJSONSupport is the direct regression guard
// for ga-ule's second symptom: `gc maintenance dolt-gc --json` was rejected
// at runtime with json_unsupported even though the flag is advertised in
// --help. With the result schemas declared, the contract manifest must now
// report json_supported=true (so the global --json gate passes the flag
// through to the command instead of rejecting it).
func TestMaintenanceCommandsDeclareJSONSupport(t *testing.T) {
	for _, command := range [][]string{
		{"maintenance", "status"},
		{"maintenance", "dolt-gc"},
	} {
		t.Run(strings.Join(command, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(append(append([]string{}, command...), "--json-schema"), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("run(%v --json-schema) = %d; stderr=%q", command, code, stderr.String())
			}
			var manifest struct {
				JSONSupported bool                       `json:"json_supported"`
				Schemas       map[string]json.RawMessage `json:"schemas"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
				t.Fatalf("manifest is not JSON: %v\n%s", err, stdout.String())
			}
			if !manifest.JSONSupported {
				t.Fatalf("json_supported=false; the advertised --json flag would still be rejected (ga-ule)\nmanifest=%s", stdout.String())
			}
			if !json.Valid(manifest.Schemas["result"]) {
				t.Fatalf("result schema missing/invalid: %s", manifest.Schemas["result"])
			}
		})
	}
}

// TestRouteMaintenanceStatus_JSONOutput verifies that --json emits a
// stable success envelope (schema_version + ok discriminator) carrying the
// status body and a _cache_age_s field, and that the payload validates
// against the declared schema. (gascity ga-ule)
func TestRouteMaintenanceStatus_JSONOutput(t *testing.T) {
	srv := httptest.NewServer(okMaintenanceStatusHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := routeMaintenanceStatus(c, "", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; want 0; stderr=%q", code, stderr.String())
	}
	payload := validateManagementJSONPayload(t, []string{"maintenance", "status"}, &stdout)
	if payload["ok"] != true || payload["schema_version"] != "1" {
		t.Errorf("missing success discriminator: %v", payload)
	}
	if _, ok := payload["_cache_age_s"]; !ok {
		t.Errorf("JSON envelope missing _cache_age_s: %v", payload)
	}
	if _, ok := payload["status"]; !ok {
		t.Errorf("JSON envelope missing status: %v", payload)
	}
}

// TestRouteMaintenanceDoltGC_JSONOutput verifies the trigger --json success
// envelope validates against the declared schema and exposes the run's
// before/after bytes so a zero-reclaim run is machine-detectable. (ga-ule)
func TestRouteMaintenanceDoltGC_JSONOutput(t *testing.T) {
	srv := httptest.NewServer(okMaintenanceTriggerWaitHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := routeMaintenanceDoltGC(c, "", true /*wait*/, true /*json*/, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; want 0; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	payload := validateManagementJSONPayload(t, []string{"maintenance", "dolt-gc"}, &stdout)
	if payload["ok"] != true || payload["schema_version"] != "1" {
		t.Errorf("missing success discriminator: %v", payload)
	}
	trigger, ok := payload["trigger"].(map[string]any)
	if !ok {
		t.Fatalf("JSON envelope missing trigger object: %v", payload)
	}
	run, ok := trigger["run"].(map[string]any)
	if !ok {
		t.Fatalf("trigger missing run object: %v", trigger)
	}
	if _, ok := run["before_bytes"]; !ok {
		t.Errorf("run missing before_bytes: %v", run)
	}
	if _, ok := run["after_bytes"]; !ok {
		t.Errorf("run missing after_bytes: %v", run)
	}
}
