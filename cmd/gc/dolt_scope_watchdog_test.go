package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestManagedDoltScopeGone(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "dolt-config.yaml")
	if err := os.WriteFile(existing, []byte("log_level: warning\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		configFile string
		want       bool
	}{
		{"existing config is alive", existing, false},
		{"missing config is gone", filepath.Join(dir, "removed", "dolt-config.yaml"), true},
		{"empty path never reaps", "", false},
		{"blank path never reaps", "   ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedDoltScopeGone(tc.configFile); got != tc.want {
				t.Errorf("managedDoltScopeGone(%q) = %v, want %v", tc.configFile, got, tc.want)
			}
		})
	}
}

func TestManagedDoltScopeWatchdogEnabledFor(t *testing.T) {
	cases := []struct {
		name     string
		testMode bool
		env      string
		want     bool
	}{
		{"production default on", false, "", true},
		{"production explicit off", false, "0", false},
		{"production explicit on", false, "1", true},
		{"test mode always off", true, "", false},
		{"test mode off even when forced", true, "1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedDoltScopeWatchdogEnabledFor(tc.testMode, tc.env); got != tc.want {
				t.Errorf("managedDoltScopeWatchdogEnabledFor(%v, %q) = %v, want %v", tc.testMode, tc.env, got, tc.want)
			}
		})
	}
}

func TestManagedDoltScopeWatchdogEnabled_OffInTestBinary(t *testing.T) {
	// The test binary is always in managed-dolt test mode, so the scope
	// watchdog must never interpose on test-spawned servers.
	if managedDoltScopeWatchdogEnabled() {
		t.Fatal("scope watchdog enabled inside the test binary; test scopes are owned by the test watchdog")
	}
}

func TestManagedDoltScopeWatchdogInterval(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", managedDoltScopeWatchdogDefaultInterval},
		{"50", 50 * time.Millisecond},
		{"0", managedDoltScopeWatchdogDefaultInterval},
		{"-5", managedDoltScopeWatchdogDefaultInterval},
		{"nonsense", managedDoltScopeWatchdogDefaultInterval},
	}
	for _, tc := range cases {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv(managedDoltScopeWatchdogIntervalEnv, tc.env)
			if got := managedDoltScopeWatchdogInterval(); got != tc.want {
				t.Errorf("interval for %q = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

// managedDoltScopeWatchdogRun is one helper-process run: the scope anchor the
// watchdog polls, the log it and its server share, and the two supervised
// PIDs. Paths are fixed under the run's directory so a caller can seed the log
// before starting.
type managedDoltScopeWatchdogRun struct {
	configPath  string
	logPath     string
	doltPID     int
	watchdogPID int
	output      []byte
}

// startManagedDoltScopeWatchdogHelper runs TestManagedDoltScopeWatchdogHelper
// in a child process, which starts a fake dolt server under a real scope
// watchdog, records both PIDs, and exits — the production lifecycle, where gc
// goes away and the watchdog keeps supervising. It returns once both PIDs are
// readable, with cleanup registered for each.
//
// The scope anchor is dir/dolt-config.yaml and the log is dir/dolt.log; extra
// entries are appended to the child's environment, which is sanitized, so
// production knobs must ride in as GC_TEST_ control vars.
func startManagedDoltScopeWatchdogHelper(t *testing.T, dir string, extraEnv ...string) managedDoltScopeWatchdogRun {
	t.Helper()
	run := managedDoltScopeWatchdogRun{
		configPath: filepath.Join(dir, "dolt-config.yaml"),
		logPath:    filepath.Join(dir, "dolt.log"),
	}
	if err := os.WriteFile(run.configPath, []byte("log_level: debug\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	statePath := filepath.Join(dir, "state")

	cmd := exec.Command(os.Args[0], "-test.run=TestManagedDoltScopeWatchdogHelper", "-test.v")
	cmd.Env = sanitizedBaseEnv(append([]string{
		"GC_TEST_MANAGED_DOLT_HELPER=scope-watchdog",
		"GC_TEST_MANAGED_DOLT_HELPER_STATE=" + statePath,
		"GC_TEST_MANAGED_DOLT_HELPER_CONFIG=" + run.configPath,
		"GC_TEST_MANAGED_DOLT_HELPER_LOG=" + run.logPath,
		"GC_TEST_MANAGED_DOLT_HELPER_FAKE_DOLT_DIR=" + writeFakeDoltSQLServer(t),
		"GC_TEST_MANAGED_DOLT_HELPER_SCOPE_WD_INTERVAL_MS=50",
	}, extraEnv...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, output)
	}
	run.output = output
	run.doltPID, run.watchdogPID = readManagedDoltTestState(t, statePath)
	t.Cleanup(func() {
		cleanupManagedDoltTestPID(t, run.doltPID)
		cleanupManagedDoltTestPID(t, run.watchdogPID)
	})
	return run
}

// TestManagedDoltScopeWatchdogKillsServerWhenScopeDeleted exercises the full
// production supervision loop: a helper process starts a fake dolt server
// under the scope watchdog, the test deletes the config file (the scope
// anchor), and the watchdog must terminate the server after the two-check
// confirmation window.
func TestManagedDoltScopeWatchdogKillsServerWhenScopeDeleted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process semantics required")
	}
	run := startManagedDoltScopeWatchdogHelper(t, t.TempDir())

	// Control window: with the config present, the server must stay alive
	// across several poll intervals — and so must its watchdog (the spawner
	// helper has already exited, the production lifecycle shape).
	time.Sleep(300 * time.Millisecond)
	if !pidAlive(run.doltPID) {
		logData, _ := os.ReadFile(run.logPath)
		t.Fatalf("fake dolt pid %d exited while scope was alive; helper output:\n%s\nwatchdog log:\n%s", run.doltPID, run.output, logData)
	}
	if !pidAlive(run.watchdogPID) {
		logData, _ := os.ReadFile(run.logPath)
		t.Fatalf("watchdog pid %d died while scope was alive; watchdog log:\n%s", run.watchdogPID, logData)
	}

	// Delete the scope anchor; the watchdog should confirm twice and reap.
	if err := os.Remove(run.configPath); err != nil {
		t.Fatalf("remove config: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for pidAlive(run.doltPID) {
		if time.Now().After(deadline) {
			logData, _ := os.ReadFile(run.logPath)
			t.Fatalf("fake dolt pid %d still alive after scope deletion; watchdog log:\n%s", run.doltPID, logData)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for pidAlive(run.watchdogPID) {
		if time.Now().After(deadline) {
			t.Fatalf("watchdog pid %d still alive after reaping its server", run.watchdogPID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	logData, _ := os.ReadFile(run.logPath)
	if !strings.Contains(string(logData), "gone for") {
		t.Errorf("watchdog log missing the scope-gone termination decision; log:\n%s", logData)
	}
}

// TestManagedDoltScopeWatchdogHelper runs in a child process: it starts a
// fake dolt server under the scope watchdog and records both PIDs, then
// exits — proving the watchdog supervises independently of its spawner,
// exactly the production lifecycle (gc exits, the watchdog stays).
func TestManagedDoltScopeWatchdogHelper(t *testing.T) {
	if os.Getenv("GC_TEST_MANAGED_DOLT_HELPER") != "scope-watchdog" {
		t.Skip("helper process only")
	}
	fakeDoltDir := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_FAKE_DOLT_DIR"))
	if fakeDoltDir == "" {
		t.Fatal("missing fake dolt dir")
	}
	t.Setenv("PATH", fakeDoltDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// TestMain scrubs non-GC_TEST_ GC_* keys from this child's environment, so
	// every production knob a caller wants to set rides in on a GC_TEST_
	// control var and is re-exported here under its real name.
	for control, production := range map[string]string{
		"GC_TEST_MANAGED_DOLT_HELPER_SCOPE_WD_INTERVAL_MS": managedDoltScopeWatchdogIntervalEnv,
		"GC_TEST_MANAGED_DOLT_HELPER_LOG_MAX_BYTES":        managedDoltLogMaxBytesEnv,
		"GC_TEST_MANAGED_DOLT_HELPER_LOG_KEEP":             managedDoltLogKeepEnv,
	} {
		if value := strings.TrimSpace(os.Getenv(control)); value != "" {
			t.Setenv(production, value)
		}
	}
	statePath := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_STATE"))
	configPath := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_CONFIG"))
	logPath := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_LOG"))
	if statePath == "" || configPath == "" || logPath == "" {
		t.Fatal("missing helper paths")
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open log file: %v", err)
	}
	defer logFile.Close() //nolint:errcheck

	started, err := startManagedDoltSQLServerWithScopeWatchdog("", configPath, logPath, logFile)
	if err != nil {
		t.Fatalf("start managed dolt with scope watchdog: %v", err)
	}
	state := fmt.Sprintf("%d %d\n", started.PID, started.WatchdogPID)
	if err := os.WriteFile(statePath, []byte(state), 0o644); err != nil {
		t.Fatalf("write helper state: %v", err)
	}
	// Opt-in: record the reported start identity so a caller test can assert the
	// scope-watchdog path populates it (the PR #4004 PID-reuse guard input).
	// Two lines: start-time ticks, then the ps-lstart identity (possibly empty).
	if identityPath := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_IDENTITY")); identityPath != "" {
		identity := fmt.Sprintf("%d\n%s\n", started.StartTimeTicks, started.StartIdentity)
		if err := os.WriteFile(identityPath, []byte(identity), 0o644); err != nil {
			t.Fatalf("write helper identity: %v", err)
		}
	}
}

// readManagedDoltScopeIdentityState parses the two-line identity file the scope
// watchdog helper writes when GC_TEST_MANAGED_DOLT_HELPER_IDENTITY is set:
// start-time ticks on line 1, the ps-lstart identity (possibly empty) on line 2.
func readManagedDoltScopeIdentityState(t *testing.T, path string) (uint64, string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read helper identity: %v", err)
	}
	lines := strings.SplitN(strings.TrimRight(string(data), "\n"), "\n", 2)
	ticks, err := strconv.ParseUint(strings.TrimSpace(lines[0]), 10, 64)
	if err != nil {
		t.Fatalf("parse helper identity ticks %q: %v", lines[0], err)
	}
	identity := ""
	if len(lines) >= 2 {
		identity = lines[1]
	}
	return ticks, identity
}

// TestManagedDoltScopeWatchdogReportsStartIdentity is the PR #4004 F1 regression
// for the production scope-watchdog path: the returned managedDoltStartedProcess
// must carry the dolt child's OS start identity, snapshotted by the watchdog
// before it can reap the child. Without it the startup-failure cleanup guard
// (terminateManagedDoltStartedProcess) falls through to unconditional bare-PID
// signaling and can kill an unrelated process that reused the numeric PID.
func TestManagedDoltScopeWatchdogReportsStartIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process semantics required")
	}
	dir := t.TempDir()
	identityPath := filepath.Join(dir, "identity")
	run := startManagedDoltScopeWatchdogHelper(t, dir, "GC_TEST_MANAGED_DOLT_HELPER_IDENTITY="+identityPath)

	ticks, identity := readManagedDoltScopeIdentityState(t, identityPath)
	if ticks == 0 && identity == "" {
		logData, _ := os.ReadFile(run.logPath)
		t.Fatalf("scope watchdog reported no start identity (ticks=%d identity=%q); PID-reuse guard disabled; log:\n%s", ticks, identity, logData)
	}
}

// TestManagedDoltScopeWatchdogServerSurvivesScopePresent asserts the
// watchdog never reaps a server whose scope stays on disk, and exits
// cleanly when the server itself goes away.
func TestManagedDoltScopeWatchdogServerSurvivesScopePresent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process semantics required")
	}
	run := startManagedDoltScopeWatchdogHelper(t, t.TempDir())

	time.Sleep(300 * time.Millisecond)
	if !pidAlive(run.doltPID) {
		logData, _ := os.ReadFile(run.logPath)
		t.Fatalf("fake dolt pid %d reaped while scope present; watchdog log:\n%s", run.doltPID, logData)
	}
	if !pidAlive(run.watchdogPID) {
		logData, _ := os.ReadFile(run.logPath)
		t.Fatalf("watchdog pid %d died while scope present; watchdog log:\n%s", run.watchdogPID, logData)
	}

	// Kill the server directly (the `gc stop` shape); the watchdog must
	// notice and exit instead of lingering.
	proc, err := os.FindProcess(run.doltPID)
	if err != nil {
		t.Fatalf("find dolt pid: %v", err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill dolt pid: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for pidAlive(run.watchdogPID) {
		if time.Now().After(deadline) {
			t.Fatalf("watchdog pid %d still alive after its server exited", run.watchdogPID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestManagedDoltScopeWatchdogRotatesOversizedLog is the ga-fyu0 regression:
// a server generation that runs for days outlives its starter, so start-time
// rotation alone cannot bound dolt.log. The watchdog is the only process still
// on the cadence, and it must enforce the size cap — copy-truncating in place
// so both its own O_APPEND handle and the server's keep writing to the same
// inode.
func TestManagedDoltScopeWatchdogRotatesOversizedLog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process semantics required")
	}
	dir := t.TempDir()
	// Stand in for the accumulated by-design reap flood: oversized, and
	// carrying a marker that must survive into the retained generation. The
	// log has to exist before the helper starts appending to it.
	const seedMarker = "SEEDED-PRE-ROTATION-CONTENT"
	seed := seedMarker + "\n" + strings.Repeat("noise line\n", 500)
	if err := os.WriteFile(filepath.Join(dir, "dolt.log"), []byte(seed), 0o644); err != nil {
		t.Fatalf("write oversized log: %v", err)
	}

	// This is also the only coverage of rotateManagedDoltLogIfOversized's env
	// wiring: both knobs travel from the environment into a real watchdog.
	run := startManagedDoltScopeWatchdogHelper(t, dir,
		"GC_TEST_MANAGED_DOLT_HELPER_LOG_MAX_BYTES=1024",
		"GC_TEST_MANAGED_DOLT_HELPER_LOG_KEEP=2",
	)

	// The generation is published by rename, so non-empty means complete.
	generation := waitForTestNonEmptyFile(t, run.logPath+".1", 10*time.Second)
	if !strings.Contains(generation, seedMarker) {
		t.Fatalf("rotated generation is missing the pre-rotation content:\n%s", generation)
	}

	live, err := os.ReadFile(run.logPath)
	if err != nil {
		t.Fatalf("read live log: %v", err)
	}
	if strings.Contains(string(live), seedMarker) {
		t.Fatalf("live log still carries pre-rotation content:\n%s", live)
	}
	if int64(len(live)) > 1024 {
		t.Fatalf("live log = %d bytes after rotation, want <= 1024", len(live))
	}
	// The watchdog must still be able to write through its own handle after
	// the truncate — the copy-truncate invariant, observed end to end.
	if !strings.Contains(string(live), "rotated "+run.logPath) {
		t.Fatalf("watchdog did not report the rotation through its own log handle:\n%s", live)
	}
	if !pidAlive(run.watchdogPID) {
		t.Fatalf("watchdog pid %d died across log rotation", run.watchdogPID)
	}
	if !pidAlive(run.doltPID) {
		t.Fatalf("fake dolt pid %d died across log rotation", run.doltPID)
	}
}

// TestRunManagedDoltScopeWatchdogUsage pins the argv contract.
func TestRunManagedDoltScopeWatchdogUsage(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close() //nolint:errcheck
	if code := runManagedDoltScopeWatchdog(nil, devnull, devnull); code != 2 {
		t.Errorf("no args exit = %d, want 2", code)
	}
	if code := runManagedDoltScopeWatchdog([]string{"a", "b"}, devnull, devnull); code != 2 {
		t.Errorf("two args exit = %d, want 2", code)
	}
	if code := runManagedDoltScopeWatchdog([]string{" ", "log", "city"}, devnull, devnull); code != 2 {
		t.Errorf("blank config exit = %d, want 2", code)
	}
}

// TestTerminateManagedDoltScopeWatchdogChildSkipsReusedPID is the PR #4004
// completeness regression for the watchdog's own reap path: the scope-gone and
// signal-forward branches terminate the dolt child through
// terminateManagedDoltScopeWatchdogChild, which must skip the signal when the
// child's numeric PID was reaped and reused (identity mismatch) while still
// terminating a child whose start identity still matches. Without the guard the
// production scope reap could SIGKILL an unrelated process that reused the PID.
func TestTerminateManagedDoltScopeWatchdogChildSkipsReusedPID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal semantics required")
	}

	// The live re-read is mocked to a fixed identity (3333); the guard compares
	// each snapshot against it, exactly as the watchdog runloop does.
	oldTicks := managedDoltTestReadStartTimeTicks
	oldIdent := managedDoltTestReadStartIdentity
	managedDoltTestReadStartTimeTicks = func(int) uint64 { return 3333 }
	managedDoltTestReadStartIdentity = func(int) string { return "" }
	t.Cleanup(func() {
		managedDoltTestReadStartTimeTicks = oldTicks
		managedDoltTestReadStartIdentity = oldIdent
	})

	// Matching snapshot (3333 == mocked re-read): the child is signaled and a
	// sleep dies on SIGTERM (a zombie reads as not-alive).
	matching := exec.Command("sleep", "60")
	if err := matching.Start(); err != nil {
		t.Fatalf("start matching child: %v", err)
	}
	matchingPID := matching.Process.Pid
	t.Cleanup(func() {
		_ = matching.Process.Kill()
		_ = matching.Wait()
	})
	if err := terminateManagedDoltScopeWatchdogChild("", matchingPID, 3333, ""); err != nil {
		t.Fatalf("guarded terminate of matching child: %v", err)
	}
	if pidAlive(matchingPID) {
		t.Fatalf("watchdog reap did not signal matching dolt child pid %d", matchingPID)
	}

	// Reused snapshot (1111 != mocked re-read 3333): the PID was reaped and the
	// number reused, so the guard must leave the live process untouched.
	reused := exec.Command("sleep", "60")
	if err := reused.Start(); err != nil {
		t.Fatalf("start reused child: %v", err)
	}
	reusedPID := reused.Process.Pid
	t.Cleanup(func() {
		_ = reused.Process.Kill()
		_ = reused.Wait()
	})
	if err := terminateManagedDoltScopeWatchdogChild("", reusedPID, 1111, ""); err != nil {
		t.Fatalf("guarded terminate of reused child: %v", err)
	}
	// Give any erroneous SIGTERM time to land before asserting survival.
	time.Sleep(200 * time.Millisecond)
	if !pidAlive(reusedPID) {
		t.Fatalf("watchdog reap signaled reused dolt child pid %d; identity guard not enforced", reusedPID)
	}
}
