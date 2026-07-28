package dolt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const logsScript = "commands/logs/run.sh"

func runLogs(t *testing.T, cityPath, host, port string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("sh", filepath.Join(root, logsScript))
	cmd.Env = append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_HOST", "GC_DOLT_PORT", "PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_HOST="+host,
		"GC_DOLT_PORT="+port,
		"PATH="+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// reapLine is one by-design connection read-timeout reap record, in the shape
// dolt emits since ga-oigp lowered read_timeout_millis to 15s: error severity,
// one line per reaped idle per-call connection. The embedded newline is
// escaped by dolt's text formatter, so each reap is a single physical line.
func reapLine(connectionID int) string {
	return `time="2026-07-27T00:36:11Z" level=error msg="Error reading packet from client ` +
		strconv.Itoa(connectionID) +
		` (127.0.0.1:54321): read tcp 127.0.0.1:3307->127.0.0.1:54321: i/o timeout\nio.ReadFull(header size) failed"`
}

// runLogsOnFile runs `gc dolt logs` against an explicit log file, so noise
// filtering can be exercised on a fixture instead of a live server.
func runLogsOnFile(t *testing.T, logPath string, args ...string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	cityPath := t.TempDir()
	cmd := exec.Command("sh", append([]string{filepath.Join(root, logsScript)}, args...)...)
	cmd.Env = append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_LOG_FILE", "PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT=3307",
		"GC_DOLT_LOG_FILE="+logPath,
		"PATH="+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func writeDoltLogFixture(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dolt.log")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write log fixture: %v", err)
	}
	return path
}

// TestLogsScriptCollapsesByDesignReapNoise is the ga-fyu0 guard. Since ga-oigp
// the read-timeout reap flood is the steady state of dolt.log, at error
// severity, which cost triage the one line that used to be the first symptom
// of the alive-but-deaf wedge. dolt owns that severity, so `gc dolt logs`
// restores the signal at the read end: collapse the by-design run into a
// single counted notice and leave every other line intact.
func TestLogsScriptCollapsesByDesignReapNoise(t *testing.T) {
	logPath := writeDoltLogFixture(t,
		"first real line",
		reapLine(1),
		reapLine(2),
		reapLine(3),
		"second real line",
	)

	out, err := runLogsOnFile(t, logPath)
	if err != nil {
		t.Fatalf("logs failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "Error reading packet from client") {
		t.Fatalf("by-design reap lines must not survive the default filter; got:\n%s", out)
	}
	if !strings.Contains(out, "suppressed 3 by-design connection read-timeout reap line(s)") {
		t.Fatalf("expected a counted suppression notice; got:\n%s", out)
	}
	if !strings.Contains(out, "--raw") {
		t.Fatalf("suppression notice must name the escape hatch; got:\n%s", out)
	}
	for _, want := range []string{"first real line", "second real line"} {
		if !strings.Contains(out, want) {
			t.Fatalf("filter dropped a real log line %q; got:\n%s", want, out)
		}
	}
	// Ordering is load-bearing for triage: the notice stands where the run was.
	firstIdx := strings.Index(out, "first real line")
	noticeIdx := strings.Index(out, "suppressed 3")
	secondIdx := strings.Index(out, "second real line")
	if firstIdx > noticeIdx || noticeIdx > secondIdx {
		t.Fatalf("suppression notice is out of position (first=%d notice=%d second=%d):\n%s", firstIdx, noticeIdx, secondIdx, out)
	}
}

// TestLogsScriptRawShowsEveryLine pins the escape hatch: --raw is the
// unfiltered tail, so nothing the filter hides is unreachable.
func TestLogsScriptRawShowsEveryLine(t *testing.T) {
	logPath := writeDoltLogFixture(t, "real line", reapLine(1), reapLine(2))

	out, err := runLogsOnFile(t, logPath, "--raw")
	if err != nil {
		t.Fatalf("logs --raw failed: %v\n%s", err, out)
	}
	if got := strings.Count(out, "Error reading packet from client"); got != 2 {
		t.Fatalf("--raw showed %d reap lines, want 2; got:\n%s", got, out)
	}
	if strings.Contains(out, "suppressed") {
		t.Fatalf("--raw must not emit the suppression notice; got:\n%s", out)
	}
	if !strings.Contains(out, "real line") {
		t.Fatalf("--raw dropped a real log line; got:\n%s", out)
	}
}

// TestLogsScriptKeepsRealWedgeSignatures is the safety half of the filter: it
// may only hide the read-timeout reap. The lines triage actually needs — the
// closed-connection signature, and any other error — must survive.
func TestLogsScriptKeepsRealWedgeSignatures(t *testing.T) {
	logPath := writeDoltLogFixture(t,
		reapLine(1),
		`time="2026-07-27T00:40:02Z" level=error msg="use of closed network connection"`,
		`time="2026-07-27T00:40:03Z" level=error msg="Error reading packet from client 9 (127.0.0.1:1): connection reset by peer"`,
		`time="2026-07-27T00:40:04Z" level=error msg="i/o timeout on backup upload"`,
	)

	out, err := runLogsOnFile(t, logPath)
	if err != nil {
		t.Fatalf("logs failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"use of closed network connection",
		"connection reset by peer",
		"i/o timeout on backup upload",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("filter swallowed a real signal %q; got:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "suppressed 1 by-design") {
		t.Fatalf("expected the single by-design reap to be suppressed; got:\n%s", out)
	}
}

// TestLogsScriptHelpDocumentsRaw keeps the escape hatch discoverable.
func TestLogsScriptHelpDocumentsRaw(t *testing.T) {
	out, err := runLogsOnFile(t, filepath.Join(t.TempDir(), "unused.log"), "--help")
	if err != nil {
		t.Fatalf("logs --help failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "--raw") {
		t.Fatalf("help does not document --raw; got:\n%s", out)
	}
}

// TestLogsScriptExternalMissingLogIsLimitationNotError is the su-deol8 guard:
// for a configured external Dolt endpoint the server log lives on the remote
// host, so a missing local dolt.log is an endpoint limitation with a clear
// message — not a hard failure the way a missing managed-server log is.
func TestLogsScriptExternalMissingLogIsLimitationNotError(t *testing.T) {
	cityPath := t.TempDir()

	out, err := runLogs(t, cityPath, "superlzy-dolt", "3306")
	if err != nil {
		t.Fatalf("logs hard-failed for external endpoint with missing local log; want exit 0 limitation: %v\n%s", err, out)
	}
	for _, want := range []string{"external Dolt endpoint", "superlzy-dolt:3306", "not available locally"} {
		if !strings.Contains(out, want) {
			t.Fatalf("logs limitation message missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "log file not found") {
		t.Fatalf("external endpoint should not emit the local managed-server 'log file not found' error:\n%s", out)
	}
}

// TestLogsScriptLocalMissingLogIsError verifies the local managed path still
// hard-fails when its expected log file is absent (unchanged behavior).
func TestLogsScriptLocalMissingLogIsError(t *testing.T) {
	cityPath := t.TempDir()

	out, err := runLogs(t, cityPath, "127.0.0.1", "3311")
	if err == nil {
		t.Fatalf("logs unexpectedly succeeded for local missing log; want error\n%s", out)
	}
	if !strings.Contains(out, "log file not found") {
		t.Fatalf("local missing-log error missing 'log file not found'; got:\n%s", out)
	}
}
