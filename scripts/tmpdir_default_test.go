package scripts_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wantTestTMPDirDefault is the fallback TMPDIR the test-running wrappers
// (Makefile TEST_ENV, and the shard scripts below) must use when the calling
// shell has not already set TMPDIR itself. It must stay off the shared,
// size-capped /tmp tmpfs (see AGENTS.md "Build Cache Conventions") and it
// must stay short: internal/testutil.ShortTempDir roots test-owned socket
// directories at os.TempDir() (== $TMPDIR on Linux), and Unix socket paths
// built under it must stay under the sun_path limit (104 bytes on macOS, 108
// on Linux; see internal/runtime/acp and internal/runtime/subprocess).
const wantTestTMPDirDefault = "/var/tmp"

// wantTestTMPDirMaxLen is the longest TMPDIR or GOTMPDIR the test runners
// pass through unchanged. Longer values are replaced with a directory
// created under /var/tmp whose path fits in this budget. 20 leaves room
// for t.TempDir's test-name suffix plus a unix socket leaf under sun_path
// (108 on Linux, 104 on macOS). /var/tmp itself is 8.
const wantTestTMPDirMaxLen = 20

// TestMakefileTestEnvDefaultsTMPDirOffSharedTmpTmpfs guards ga-ntbpyb.4: make
// test-fast-parallel (and every other $(TEST_ENV)-wrapped target) must not
// fall back to the shared /tmp tmpfs when the caller leaves TMPDIR unset.
func TestMakefileTestEnvDefaultsTMPDirOffSharedTmpTmpfs(t *testing.T) {
	got := runMakefileTestEnvTMPDirPrintTarget(t, nil)
	if got == "/tmp" || strings.HasPrefix(got, "/tmp/") {
		t.Fatalf("TEST_ENV TMPDIR = %q, still rooted under the shared /tmp tmpfs", got)
	}
	if got != wantTestTMPDirDefault {
		t.Fatalf("TEST_ENV TMPDIR = %q, want %q", got, wantTestTMPDirDefault)
	}
}

// TestMakefileTestEnvRespectsCallerSuppliedTMPDir guards the other half of
// the same fallback expression: a caller (CI, a developer's shell, a deploy
// gate) that already exports a short TMPDIR must still have that value win.
// "Short" means at most wantTestTMPDirMaxLen bytes, the sun_path budget the
// runners enforce. A longer caller path is covered by
// TestMakefileTestEnvShortensOverlongTMPDIR.
func TestMakefileTestEnvRespectsCallerSuppliedTMPDir(t *testing.T) {
	custom := shortCallerTMPDir(t)
	got := runMakefileTestEnvTMPDirPrintTarget(t, []string{"TMPDIR=" + custom})
	if got != custom {
		t.Fatalf("TEST_ENV TMPDIR = %q, want caller-supplied %q", got, custom)
	}
}

// TestMakefileTestEnvShortensOverlongTMPDIR is the refinery-gate case
// (ga-7g3k). Convoy and bisect gates export TMPDIR to a long directory under
// /var/tmp (gc-refinery-cache / gc-refinery-bisect, ~40 bytes). Unix socket
// paths built from os.TempDir then exceed sun_path and fail on a clean tree.
// The test process must see a short directory instead, and that directory
// must be removed when the recipe exits. The caller's long directory stays.
func TestMakefileTestEnvShortensOverlongTMPDIR(t *testing.T) {
	long := longCallerTMPDir(t)
	got := runMakefileTestEnvTMPDirPrintTarget(t, []string{"TMPDIR=" + long})
	assertShortReplacementTMPDir(t, got, long)
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatalf("short TMPDIR %q still exists after TEST_ENV returned (err=%v); the runner must remove a directory it created", got, err)
	}
	if _, err := os.Stat(long); err != nil {
		t.Fatalf("caller TMPDIR %q was removed: %v", long, err)
	}
}

// TestMakefileTestEnvShortensOverlongGOTMPDIRKeepsGOCACHE covers Go 1.26,
// where testing.T.TempDir prefers GOTMPDIR over TMPDIR. A long GOTMPDIR has
// the same sun_path failure as a long TMPDIR. GOCACHE must stay on the
// caller path: the build cache is allowed to be a long on-disk directory
// and must not be relocated onto the short temp dir or onto /tmp.
func TestMakefileTestEnvShortensOverlongGOTMPDIRKeepsGOCACHE(t *testing.T) {
	long := longCallerTMPDir(t)
	cache := filepath.Join(long, "cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	shortTMP := shortCallerTMPDir(t)
	got := runMakefileTestEnvPrintTarget(t, []string{
		"TMPDIR=" + shortTMP,
		"GOTMPDIR=" + long,
		"GOCACHE=" + cache,
	})
	if got["TMPDIR"] != shortTMP {
		t.Fatalf("TMPDIR = %q, want short caller value %q", got["TMPDIR"], shortTMP)
	}
	assertShortReplacementTMPDir(t, got["GOTMPDIR"], long)
	if got["GOCACHE"] != cache {
		t.Fatalf("GOCACHE = %q, want caller cache %q", got["GOCACHE"], cache)
	}
	if _, err := os.Stat(got["GOTMPDIR"]); !os.IsNotExist(err) {
		t.Fatalf("short GOTMPDIR %q still exists after TEST_ENV returned (err=%v)", got["GOTMPDIR"], err)
	}
}

// TestMakefileTestEnvTMPDirDefaultLeavesSocketPathHeadroom proves the actual
// resolved default (not just an assumed literal) leaves enough room for a
// realistic Unix socket path. Mirrors the "socks/<hashed-key>.sock" shape
// built by internal/runtime/subprocess.Provider.sockPath and
// internal/runtime/acp.Provider.sockPath: a short prefix directory (per
// internal/testutil.ShortTempDir) holding a "socks" dir and a 9-byte hashed
// key ("s" + 8 hex chars) plus ".sock".
func TestMakefileTestEnvTMPDirDefaultLeavesSocketPathHeadroom(t *testing.T) {
	root := runMakefileTestEnvTMPDirPrintTarget(t, nil)
	shortDir := filepath.Join(root, "gc-t-123456789")
	sockPath := filepath.Join(shortDir, "socks", "s01234567.sock")
	const sunPathLimit = 104 // stricter of macOS(104)/Linux(108) sun_path limits
	const wantHeadroom = 20  // arbitrary but generous safety margin in bytes
	if margin := sunPathLimit - len(sockPath); margin < wantHeadroom {
		t.Fatalf("socket path %q (%d bytes) leaves only %d bytes of headroom under the sun_path limit %d; want >= %d",
			sockPath, len(sockPath), margin, sunPathLimit, wantHeadroom)
	}
}

func runMakefileTestEnvTMPDirPrintTarget(t *testing.T, extraEnv []string) string {
	t.Helper()
	got := runMakefileTestEnvPrintTarget(t, extraEnv)
	tmpdir, ok := got["TMPDIR"]
	if !ok {
		t.Fatalf("print-test-env-tmpdir produced no TMPDIR: %#v", got)
	}
	return tmpdir
}

func runMakefileTestEnvPrintTarget(t *testing.T, extraEnv []string) map[string]string {
	t.Helper()
	repoRoot := repoRoot(t)
	makefile, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	tmp := t.TempDir()
	testMakefile := filepath.Join(tmp, "Makefile")
	content := string(makefile) + `
.PHONY: print-test-env-tmpdir
print-test-env-tmpdir:
	@$(TEST_ENV) sh -c 'printf "TMPDIR=%s\nGOTMPDIR=%s\nGOCACHE=%s\n" "$$TMPDIR" "$$GOTMPDIR" "$$GOCACHE"'
`
	if err := os.WriteFile(testMakefile, []byte(content), 0o644); err != nil {
		t.Fatalf("write test Makefile: %v", err)
	}

	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"USER=" + os.Getenv("USER"),
		"SHELL=/bin/sh",
		// Makefile-internal `go env` probes must not download a toolchain
		// into an isolated HOME.
		"GOTOOLCHAIN=local",
	}
	env = append(env, extraEnv...)

	cmd := makeCommand("--no-print-directory", "-f", testMakefile, "print-test-env-tmpdir")
	cmd.Dir = repoRoot
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make print-test-env-tmpdir failed: %v\n%s", err, out)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok || name == "" {
			t.Fatalf("unexpected output from print-test-env-tmpdir: %q", out)
		}
		got[name] = value
	}
	return got
}

func shortCallerTMPDir(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("s%04x", os.Getpid()&0xFFFF)
	dir := filepath.Join("/var/tmp", name)
	if len(dir) > wantTestTMPDirMaxLen {
		t.Fatalf("short caller TMPDIR %q is %d bytes, want <= %d", dir, len(dir), wantTestTMPDirMaxLen)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir short caller TMPDIR: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func longCallerTMPDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/var/tmp", "gc-refinery-bisect-ga7.")
	if err != nil {
		t.Fatalf("mkdir long caller TMPDIR: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if len(dir) <= wantTestTMPDirMaxLen {
		t.Fatalf("long caller TMPDIR %q is %d bytes, want > %d", dir, len(dir), wantTestTMPDirMaxLen)
	}
	return dir
}

// TestShortTestTMPDirMarkerStaysInTheRunner stops a nested shard from
// deleting the directory its parent still has go test pointed at. The
// marker is how the EXIT trap finds that directory, so it must not cross
// a process boundary.
func TestShortTestTMPDirMarkerStaysInTheRunner(t *testing.T) {
	repo := repoRoot(t)
	long := longCallerTMPDir(t)
	var marker string
	t.Cleanup(func() {
		if marker != "" {
			_ = os.RemoveAll(marker)
		}
	})
	// testCommand already owns the package's os/exec call. A direct
	// exec.Command here would grow the untagged subprocess census (ga-7g3k).
	cmd := testCommand("bash", "-c", `
set -euo pipefail
source "$1"
export TMPDIR="$2"
gc_pin_short_test_tmpdir
marker="${GC_TEST_SHORT_TMPDIR:-}"
test -n "$marker"
test -d "$marker"
# A nested runner inherits the environment, not unexported shell variables.
inherited=$(bash -c 'printf %s "${GC_TEST_SHORT_TMPDIR-unset}"')
printf 'marker=%s\ninherited=%s\n' "$marker" "$inherited"
`, "pin", filepath.Join(repo, "scripts/lib/short-test-tmpdir.sh"), long)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pin helper failed: %v\n%s", err, out)
	}
	var inherited string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("unexpected pin output: %q", out)
		}
		switch name {
		case "marker":
			marker = value
		case "inherited":
			inherited = value
		}
	}
	assertShortReplacementTMPDir(t, marker, long)
	if inherited != "unset" {
		t.Fatalf("child inherited GC_TEST_SHORT_TMPDIR=%q; nested runners would delete the parent temp dir", inherited)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("short dir %q disappeared before the parent cleaned it up: %v", marker, err)
	}
}

func assertShortReplacementTMPDir(t *testing.T, got, long string) {
	t.Helper()
	if got == long {
		t.Fatalf("temp path %q was passed through; a path longer than %d bytes must be replaced", got, wantTestTMPDirMaxLen)
	}
	if got == "/tmp" || strings.HasPrefix(got, "/tmp/") {
		t.Fatalf("replacement TMPDIR %q is on the shared /tmp tmpfs", got)
	}
	if !strings.HasPrefix(got, "/var/tmp/") {
		t.Fatalf("replacement TMPDIR %q, want a directory under /var/tmp", got)
	}
	if len(got) > wantTestTMPDirMaxLen {
		t.Fatalf("replacement TMPDIR %q is %d bytes, want <= %d", got, len(got), wantTestTMPDirMaxLen)
	}
}

// shardScriptTMPDirDefaults documents every sharded/parallel test-runner
// script that constructs its own env -i wrapper around go test (mirroring
// the Makefile's TEST_ENV) and must therefore apply the same off-tmpfs
// TMPDIR default. Each count is the exact number of "${TMPDIR:-...}"
// fallback sites in that file today; a changed count means a site was added
// or removed and this ledger must be updated deliberately, not silently.
var shardScriptTMPDirDefaults = map[string]int{
	"scripts/test-local-parallel":    2, // log_dir mktemp + per-job env
	"scripts/go-test-observable":     1, // per-run log file mktemp
	"scripts/test-go-test-shard":     1, // per-shard env
	"scripts/test-integration-shard": 1, // per-shard env
}

// TestShardScriptsDefaultTMPDirOffSharedTmpTmpfs is the sibling-targets half
// of ga-ntbpyb.4: test-cmd-gc-process-parallel, test-integration-shards-parallel,
// and test-local-full-parallel all fan out through these scripts directly
// (not through the Makefile's TEST_ENV), so each script's own TMPDIR fallback
// must independently stay off /tmp.
func TestShardScriptsDefaultTMPDirOffSharedTmpTmpfs(t *testing.T) {
	repoRoot := repoRoot(t)
	oldPattern := "${TMPDIR:-/tmp}"
	newPattern := "${TMPDIR:-" + wantTestTMPDirDefault + "}"
	for relPath, wantCount := range shardScriptTMPDirDefaults {
		t.Run(relPath, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(repoRoot, relPath))
			if err != nil {
				t.Fatalf("read %s: %v", relPath, err)
			}
			content := string(data)
			if strings.Contains(content, oldPattern) {
				t.Fatalf("%s still falls back to the shared /tmp tmpfs via %q", relPath, oldPattern)
			}
			if got := strings.Count(content, newPattern); got != wantCount {
				t.Fatalf("%s has %d occurrences of %q, want %d", relPath, got, newPattern, wantCount)
			}
		})
	}
}
