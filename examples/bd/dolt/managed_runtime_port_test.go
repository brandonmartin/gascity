package dolt_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// managed_runtime_port decides whether the managed Dolt server is up, and
// therefore whether every `gc dolt` subcommand can resolve a port at all. The
// recorded pid in dolt-state.json goes stale on every server restart, so
// liveness must come from live state — a listener on the recorded port —
// rather than from the recorded pid. Trusting the pid turns a healthy server
// into a false outage that also disables the Dolt incident-diagnostic protocol
// (`gc dolt sql`, `gc dolt logs`, `gc dolt health`), pushing operators toward
// the blind restart the protocol exists to prevent (ga-pu2s).
//
// The logic is duplicated in the Dolt pack's runtime.sh and in core's
// standalone dolt-target.sh. Both copies run the same table so the liveness
// semantics cannot drift apart.

const (
	// Pids far above any realistic pid_max, so `kill -0` always fails and the
	// fake `ps` below is the sole authority on liveness. Without that the host
	// process table would leak into the test.
	fakeRecordedPID = "99999991"
	fakeLivePID     = "99999992"
	fakeForeignPID  = "99999993"
	fakeDeadPID     = "99999994"

	fakePort = "47931"
)

// fakeProc describes one process the fake `ps` reports as running.
type fakeProc struct {
	pid  string
	comm string
	args string
}

func doltProc(pid string) fakeProc {
	return fakeProc{pid: pid, comm: "dolt", args: "dolt sql-server --config /tmp/dolt-config.yaml"}
}

type managedPortCase struct {
	name string
	// recordedPID is written into the state fixture; empty omits the field.
	recordedPID string
	// running is the state file's running flag.
	running bool
	// dataDirMatches controls whether the state file's data_dir matches the
	// data dir the caller expects.
	dataDirMatches bool
	// listenerPID is what the fake `lsof` reports as holding fakePort; empty
	// means no listener could be enumerated.
	listenerPID string
	// procs is the set of processes the fake `ps` reports as alive.
	procs []fakeProc
	// tcpReachable controls the fake `nc` exit status.
	tcpReachable bool

	wantPort string
	// wantStderrContains are substrings the stale-state note must carry, so
	// an operator reading the warning learns which pid is actually serving.
	wantStderrContains []string
	wantSilent         bool
}

func managedPortCases() []managedPortCase {
	return []managedPortCase{
		{
			name:           "live listener matches recorded pid",
			recordedPID:    fakeRecordedPID,
			running:        true,
			dataDirMatches: true,
			listenerPID:    fakeRecordedPID,
			procs:          []fakeProc{doltProc(fakeRecordedPID)},
			wantPort:       fakePort,
			wantSilent:     true,
		},
		{
			// The reported regression: the recorded pid is dead but a healthy
			// dolt server is listening on the recorded port.
			name:               "stale recorded pid with live dolt listener",
			recordedPID:        fakeDeadPID,
			running:            true,
			dataDirMatches:     true,
			listenerPID:        fakeLivePID,
			procs:              []fakeProc{doltProc(fakeLivePID)},
			wantPort:           fakePort,
			wantStderrContains: []string{"stale", fakeDeadPID, fakeLivePID, fakePort},
		},
		{
			// Pid reuse: the recorded pid is alive but is not the server.
			name:               "recorded pid alive but a different dolt server holds the port",
			recordedPID:        fakeRecordedPID,
			running:            true,
			dataDirMatches:     true,
			listenerPID:        fakeLivePID,
			procs:              []fakeProc{doltProc(fakeLivePID), {pid: fakeRecordedPID, comm: "sshd", args: "sshd: unrelated"}},
			wantPort:           fakePort,
			wantStderrContains: []string{"stale", fakeRecordedPID, fakeLivePID},
		},
		{
			// An unrelated process claimed the port after the server died.
			// Resolving it would point every `gc dolt` call at a foreign
			// service, so the port must stay unresolved.
			name:           "foreign process holds the recorded port",
			recordedPID:    fakeDeadPID,
			running:        true,
			dataDirMatches: true,
			listenerPID:    fakeForeignPID,
			procs:          []fakeProc{{pid: fakeForeignPID, comm: "nginx", args: "nginx: master process"}},
			wantPort:       "",
		},
		{
			// lsof is unavailable or the holder belongs to another user, so
			// the listener cannot be enumerated. Connecting still proves the
			// server is up.
			name:               "no enumerable listener but port is reachable",
			recordedPID:        fakeDeadPID,
			running:            true,
			dataDirMatches:     true,
			listenerPID:        "",
			procs:              nil,
			tcpReachable:       true,
			wantPort:           fakePort,
			wantStderrContains: []string{"stale", fakeDeadPID, fakePort},
		},
		{
			name:           "no listener and port unreachable",
			recordedPID:    fakeDeadPID,
			running:        true,
			dataDirMatches: true,
			listenerPID:    "",
			procs:          nil,
			tcpReachable:   false,
			wantPort:       "",
		},
		{
			name:           "state records not running",
			recordedPID:    fakeRecordedPID,
			running:        false,
			dataDirMatches: true,
			listenerPID:    fakeRecordedPID,
			procs:          []fakeProc{doltProc(fakeRecordedPID)},
			wantPort:       "",
		},
		{
			// A state file describing a different data dir belongs to another
			// city; its port must never be adopted.
			name:           "data dir mismatch",
			recordedPID:    fakeRecordedPID,
			running:        true,
			dataDirMatches: false,
			listenerPID:    fakeRecordedPID,
			procs:          []fakeProc{doltProc(fakeRecordedPID)},
			wantPort:       "",
		},
	}
}

func TestManagedRuntimePortRuntimeSh(t *testing.T) {
	root := repoRoot(t)
	runManagedPortCases(t, filepath.Join(root, "assets", "scripts", "runtime.sh"))
}

func TestManagedRuntimePortDoltTargetSh(t *testing.T) {
	root := repoRoot(t)
	runManagedPortCases(t, filepath.Join(root, "..", "..", "..", "internal", "bootstrap", "packs", "core", "assets", "scripts", "dolt-target.sh"))
}

func runManagedPortCases(t *testing.T, scriptPath string) {
	t.Helper()
	prefix := scriptThroughManagedRuntimePort(t, scriptPath)

	for _, tc := range managedPortCases() {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr := runManagedRuntimePort(t, prefix, tc)

			wantStdout := ""
			if tc.wantPort != "" {
				wantStdout = tc.wantPort + "\n"
			}
			if stdout != wantStdout {
				t.Fatalf("stdout = %q, want %q\nstderr:\n%s", stdout, wantStdout, stderr)
			}
			for _, want := range tc.wantStderrContains {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr does not mention %q\nstderr:\n%s", want, stderr)
				}
			}
			if tc.wantSilent && stderr != "" {
				t.Fatalf("stderr = %q, want empty", stderr)
			}
		})
	}
}

// runManagedRuntimePort evaluates the extracted script prefix and calls
// managed_runtime_port against the fixture, with lsof/ps/nc faked so the test
// never depends on the host's real ports or process table.
func runManagedRuntimePort(t *testing.T, prefix string, tc managedPortCase) (string, string) {
	t.Helper()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, ".beads", "dolt")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	stateDataDir := dataDir
	if !tc.dataDirMatches {
		stateDataDir = filepath.Join(t.TempDir(), "other-city", ".beads", "dolt")
	}

	pidField := ""
	if tc.recordedPID != "" {
		pidField = fmt.Sprintf(`"pid":%s,`, tc.recordedPID)
	}
	stateFile := filepath.Join(t.TempDir(), "dolt-state.json")
	state := fmt.Sprintf(`{"running":%t,%s"port":%s,"data_dir":%q}`, tc.running, pidField, fakePort, stateDataDir)
	if err := os.WriteFile(stateFile, []byte(state), 0o644); err != nil {
		t.Fatalf("write state fixture: %v", err)
	}

	fakeBin := t.TempDir()
	writeExecutable(t, filepath.Join(fakeBin, "lsof"), fakeLsofScript(tc.listenerPID))
	writeExecutable(t, filepath.Join(fakeBin, "ps"), fakePsScript(tc.procs))
	ncExit := 1
	if tc.tcpReachable {
		ncExit = 0
	}
	writeExecutable(t, filepath.Join(fakeBin, "nc"), fmt.Sprintf("#!/bin/sh\nexit %d\n", ncExit))

	driver := prefix + "\nmanaged_runtime_port \"$STATE_FILE\" \"$DATA_DIR\"\n"

	cmd := exec.Command("sh", "-c", driver)
	cmd.Env = append(
		filteredEnv("PATH", "STATE_FILE", "DATA_DIR"),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"STATE_FILE="+stateFile,
		"DATA_DIR="+dataDir,
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		exitErr := &exec.ExitError{}
		if !errors.As(err, &exitErr) {
			t.Fatalf("driver failed to run: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		t.Fatalf("driver exited %d, want 0\nstdout:\n%s\nstderr:\n%s", exitErr.ExitCode(), stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}

// fakeLsofScript reports holderPID as the listener for any -iTCP query, or
// nothing when holderPID is empty (no enumerable listener).
func fakeLsofScript(holderPID string) string {
	if holderPID == "" {
		return "#!/bin/sh\nexit 1\n"
	}
	return fmt.Sprintf("#!/bin/sh\nfor arg in \"$@\"; do\n  case \"$arg\" in\n    -iTCP:%s) echo %s; exit 0 ;;\n  esac\ndone\nexit 1\n", fakePort, holderPID)
}

// fakePsScript answers the three query forms the runtime helpers use:
// `-p <pid> -o pid=` (liveness), `-o comm=` (process name) and `-o args=`
// (command line). Unknown pids exit non-zero, i.e. are not running.
func fakePsScript(procs []fakeProc) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("[ \"$1\" = \"-p\" ] || exit 1\n")
	b.WriteString("pid=\"$2\"\n")
	b.WriteString("case \"$pid\" in\n")
	for _, p := range procs {
		fmt.Fprintf(&b, "  %s) comm=%q; args=%q ;;\n", p.pid, p.comm, p.args)
	}
	b.WriteString("  *) exit 1 ;;\n")
	b.WriteString("esac\n")
	b.WriteString("case \"$4\" in\n")
	b.WriteString("  pid=) printf ' %s\\n' \"$pid\" ;;\n")
	b.WriteString("  comm=) printf '%s\\n' \"$comm\" ;;\n")
	b.WriteString("  args=) printf '%s\\n' \"$args\" ;;\n")
	b.WriteString("  *) exit 1 ;;\n")
	b.WriteString("esac\n")
	return b.String()
}

// scriptThroughManagedRuntimePort returns the leading portion of a shell
// script up to and including its managed_runtime_port definition. Sourcing the
// whole script is not an option: both scripts resolve a port and exit at the
// bottom, which is the very behavior under test.
func scriptThroughManagedRuntimePort(t *testing.T, scriptPath string) string {
	t.Helper()
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", scriptPath, err)
	}
	lines := strings.Split(string(data), "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "managed_runtime_port() ") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s: no managed_runtime_port definition found", scriptPath)
	}
	for i := start + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == ")" {
			return strings.Join(lines[:i+1], "\n")
		}
	}
	t.Fatalf("%s: managed_runtime_port definition is not terminated", scriptPath)
	return ""
}
