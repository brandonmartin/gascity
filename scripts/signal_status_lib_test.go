package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// signalStatusLib is the one definition of "this run was killed, not failed"
// shared by every push-gate layer that reports a suite outcome.
const signalStatusLib = "scripts/lib/signal-status.sh"

// runSignalStatusLib sources the library and runs body, returning its combined
// output and exit status.
func runSignalStatusLib(t *testing.T, body string) (string, int) {
	t.Helper()
	lib := filepath.Join(repoRoot(t), signalStatusLib)
	cmd := exec.Command("bash", "-c", ". "+lib+"\n"+body)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exitErr *exec.ExitError
	if !asExitError(err, &exitErr) {
		t.Fatalf("run %s: %v\n%s", signalStatusLib, err, out)
	}
	return string(out), exitErr.ExitCode()
}

// TestSignalStatusRecognizesDirectSignalDeath covers the wait status a shell
// reports when its child died to a signal: 128+N. This is the case that must
// never read as a test failure — no test produced a verdict.
func TestSignalStatusRecognizesDirectSignalDeath(t *testing.T) {
	for status, wantSignal := range map[string]string{
		"129": "1",  // SIGHUP
		"130": "2",  // SIGINT
		"137": "9",  // SIGKILL
		"143": "15", // SIGTERM — the harness/teardown reap this gate keeps meeting
	} {
		out, code := runSignalStatusLib(t, "gc_status_is_signal_death "+status+" && gc_signal_status_describe "+status)
		if code != 0 {
			t.Errorf("status %s not classified as signal death (exit %d): %s", status, code, out)
			continue
		}
		if !strings.Contains(out, wantSignal) {
			t.Errorf("describe(%s) = %q, want it to name signal %s", status, out, wantSignal)
		}
		if !strings.Contains(strings.ToLower(out), "kill") {
			t.Errorf("describe(%s) = %q, want it to say the run was killed", status, out)
		}
	}
}

// TestSignalStatusRecognizesXargsSignalRelay covers the parallel runner's
// shape: the fan-out is an `xargs -P`, and GNU xargs collapses "a command was
// killed by a signal" to its own exit 125 rather than passing 128+N through.
// Without this case a signal-killed job reaches the epilogue as a plain
// nonzero and is reported as a failing test.
func TestSignalStatusRecognizesXargsSignalRelay(t *testing.T) {
	out, code := runSignalStatusLib(t, "gc_status_is_signal_death 125 && gc_signal_status_describe 125")
	if code != 0 {
		t.Fatalf("xargs signal relay (125) not classified as signal death (exit %d): %s", code, out)
	}
	if !strings.Contains(strings.ToLower(out), "kill") {
		t.Fatalf("describe(125) = %q, want it to say the run was killed", out)
	}
}

// TestSignalStatusLeavesOrdinaryFailuresAlone is the other half of the
// contract: a real test failure, and the push gate's own EX_TEMPFAIL slot
// exhaustion, must keep reading as themselves.
func TestSignalStatusLeavesOrdinaryFailuresAlone(t *testing.T) {
	for _, status := range []string{"0", "1", "2", "75", "123", "124", "126", "127"} {
		if out, code := runSignalStatusLib(t, "gc_status_is_signal_death "+status); code == 0 {
			t.Errorf("status %s classified as signal death, want ordinary: %s", status, out)
		}
		if out, code := runSignalStatusLib(t, "gc_signal_status_describe "+status); code == 0 {
			t.Errorf("describe(%s) produced %q for an ordinary status, want no description", status, out)
		}
	}
}

// TestSignalStatusRejectsNonNumericStatus keeps a malformed status from being
// silently reported as a kill, which would excuse a genuine failure.
func TestSignalStatusRejectsNonNumericStatus(t *testing.T) {
	for _, status := range []string{"", "abc", "12x", "-1"} {
		if out, code := runSignalStatusLib(t, "gc_status_is_signal_death '"+status+"'"); code == 0 {
			t.Errorf("status %q classified as signal death, want rejection: %s", status, out)
		}
	}
}

// TestReportJobOutcomeNamesAKilledJob covers the per-job line in the parallel
// fan-out. Naming which job was killed — and that it produced no verdict — is
// what separates "one shard was reaped" from "one shard has a failing test".
func TestReportJobOutcomeNamesAKilledJob(t *testing.T) {
	out, code := runSignalStatusLib(t, `gc_report_job_outcome unit-core 143 /tmp/unit-core.log`)
	if code != 0 {
		t.Fatalf("gc_report_job_outcome exit = %d: %s", code, out)
	}
	for _, want := range []string{"unit-core", "KILLED", "15", "/tmp/unit-core.log"} {
		if !strings.Contains(out, want) {
			t.Errorf("job outcome %q missing %q", out, want)
		}
	}
	if strings.Contains(out, "failed with exit") {
		t.Errorf("killed job reported as a failure: %q", out)
	}
}

// TestReportJobOutcomeKeepsFailureWordingForRealFailures pins the existing
// line for the ordinary case, so the new classification cannot quietly
// relabel genuine failures.
func TestReportJobOutcomeKeepsFailureWordingForRealFailures(t *testing.T) {
	out, code := runSignalStatusLib(t, `gc_report_job_outcome cmd-gc-3-of-6 1 /tmp/shard.log`)
	if code != 0 {
		t.Fatalf("gc_report_job_outcome exit = %d: %s", code, out)
	}
	want := "[cmd-gc-3-of-6] failed with exit 1; log: /tmp/shard.log"
	if strings.TrimSpace(out) != want {
		t.Fatalf("job outcome = %q, want %q", strings.TrimSpace(out), want)
	}
}

// TestReportSuiteOutcomeDistinguishesAKilledSweep covers the runner's closing
// verdict — the line an agent actually reads when the gate ends. Under
// ga-8qmy it said "One or more fast jobs failed" for a sweep that was killed
// with zero FAIL lines anywhere in the logs.
func TestReportSuiteOutcomeDistinguishesAKilledSweep(t *testing.T) {
	// 125 is how a signal-killed job reaches this layer through `xargs -P`.
	for _, status := range []string{"125", "143"} {
		out, code := runSignalStatusLib(t, `gc_report_suite_outcome fast `+status+` /tmp/logs`)
		if code != 0 {
			t.Fatalf("gc_report_suite_outcome %s exit = %d: %s", status, code, out)
		}
		lower := strings.ToLower(out)
		if !strings.Contains(lower, "kill") {
			t.Errorf("suite outcome for %s does not say it was killed: %q", status, out)
		}
		if !strings.Contains(lower, "not a test failure") {
			t.Errorf("suite outcome for %s does not distinguish a kill from a failure: %q", status, out)
		}
		if !strings.Contains(out, "/tmp/logs") {
			t.Errorf("suite outcome for %s drops the log directory: %q", status, out)
		}
	}
}

// TestReportSuiteOutcomeKeepsFailureWordingForRealFailures pins the
// pre-existing closing line for a suite that ran and failed.
func TestReportSuiteOutcomeKeepsFailureWordingForRealFailures(t *testing.T) {
	out, code := runSignalStatusLib(t, `gc_report_suite_outcome integration 1 /tmp/logs`)
	if code != 0 {
		t.Fatalf("gc_report_suite_outcome exit = %d: %s", code, out)
	}
	want := "One or more integration jobs failed; logs are in /tmp/logs"
	if strings.TrimSpace(out) != want {
		t.Fatalf("suite outcome = %q, want %q", strings.TrimSpace(out), want)
	}
}

// TestGateLayersRouteFailureReportingThroughTheSharedLibrary guards the
// wiring, not the wording. Both push-gate layers that print an outcome must
// classify it first; a bare `echo ... failed` reintroduces the ga-8qmy
// misreading one edit at a time, and it would not fail any other test here.
func TestGateLayersRouteFailureReportingThroughTheSharedLibrary(t *testing.T) {
	root := repoRoot(t)
	for path, wants := range map[string][]string{
		"scripts/test-local-parallel": {
			"lib/signal-status.sh",
			"gc_report_job_outcome",
			"gc_report_suite_outcome",
		},
		".githooks/pre-push": {
			"lib/signal-status.sh",
			"gc_status_is_signal_death",
		},
	} {
		body, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, want := range wants {
			if !strings.Contains(string(body), want) {
				t.Errorf("%s no longer references %q; killed runs will read as test failures again", path, want)
			}
		}
	}
}

// TestPrePushRunsTheGateAsAChild pins the shape that makes the diagnostic
// reachable at all. `exec make ...` replaces this hook with make, leaving
// nothing alive to classify how the suite ended.
func TestPrePushRunsTheGateAsAChild(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), ".githooks/pre-push"))
	if err != nil {
		t.Fatalf("read pre-push: %v", err)
	}
	if strings.Contains(string(body), "exec make") {
		t.Fatal("pre-push execs the push-time suite again; the hook must outlive it to report a signal death (ga-8qmy)")
	}
}

// TestTestingMdDescribesPrePushAsChild keeps the push-gate slots prose from
// drifting back to "exec make test-fast-parallel" after ga-8qmy. The same
// document already states the child-process shape later; this pins the
// earlier path so the two cannot contradict again (ga-mj7w).
func TestTestingMdDescribesPrePushAsChild(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "TESTING.md"))
	if err != nil {
		t.Fatalf("read TESTING.md: %v", err)
	}
	text := string(body)
	// Stale parenthetical left by ga-8qmy's own TESTING.md addition.
	if strings.Contains(text, "(`exec make test-fast-parallel`)") {
		t.Fatal("TESTING.md still describes pre-push as `exec make test-fast-parallel`; it must wait on make as a child (ga-8qmy / ga-mj7w)")
	}
	// Positive anchor: the corrected slots section and the signal-status
	// section both need the child wording so a partial rewrite cannot
	// reintroduce the contradiction by deleting only one of them.
	if !strings.Contains(text, "waits on `make test-fast-parallel` as a child") {
		t.Fatal("TESTING.md push-gate slots section must say pre-push waits on make as a child")
	}
	if !strings.Contains(text, "waits on `make test-fast-parallel` instead of `exec`ing") {
		t.Fatal("TESTING.md signal-status section must keep the child-process wording from ga-8qmy")
	}
}
