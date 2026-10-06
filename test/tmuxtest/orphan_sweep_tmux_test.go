package tmuxtest

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/pidutil"
)

// TestSweepOrphanPIDPrefixedDirsKillsLiveTmuxServerBeforeRemoval covers the
// gap behind ga-t33q83: a tmux server whose socket lives inside a
// PID-prefixed socket parent dir must not survive that dir being swept.
// SweepOrphanPIDPrefixedDirs currently removes the dir with os.RemoveAll
// once its creator is confirmed dead, but never checks for or kills a tmux
// server still bound to a socket underneath it first -- RemoveAll unlinks
// the socket file out from under the server without touching the server
// process, leaving it running and orphaned (matching the "reparented to
// systemd" symptom in the bug report).
//
// This spawns a real tmux server inside a synthetic dead-creator fixture
// (the same fixture shape TestSweepOrphanPIDPrefixedDirsRemovesStaleDeadPIDWithNilDiagnostics
// uses) and asserts the server process itself -- checked by PID via
// pidutil.Alive, not by socket reachability, since the socket path is
// unlinked by the sweep regardless of whether the server process survives
// -- is gone after the sweep runs.
func TestSweepOrphanPIDPrefixedDirsKillsLiveTmuxServerBeforeRemoval(t *testing.T) {
	RequireTmux(t)

	// Unix domain socket paths are capped at ~108 bytes (sizeof sun_path).
	// t.TempDir() embeds the full test name and is too long for that limit,
	// so this uses a short root directly under /tmp -- the same pattern
	// (and the same path-length reason) production's own
	// NewSocketParentDir("/tmp", ...) call sites use.
	root, err := os.MkdirTemp("/tmp", "tmuxtest-orphan-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	dir := pidPrefixedTestDir(t, root, "pfx-", nonLivePID(t))

	socket := filepath.Join(dir, "sock")
	if out, err := exec.Command("tmux", "-S", socket, "new-session", "-d", "-s", "orphantest", "sleep", "60").CombinedOutput(); err != nil {
		t.Fatalf("starting real tmux server at %s: %v: %s", socket, err, out)
	}

	pidOut, err := exec.Command("tmux", "-S", socket, "display-message", "-p", "-t", "orphantest", "#{pid}").Output()
	if err != nil {
		t.Fatalf("querying tmux server pid: %v", err)
	}
	serverPID, err := strconv.Atoi(strings.TrimSpace(string(pidOut)))
	if err != nil {
		t.Fatalf("parsing tmux server pid %q: %v", pidOut, err)
	}
	// Capture the start-time identity token now, while the PID is known to
	// still be this server: by cleanup time the process is expected to be
	// dead and its PID reusable, so every later check has to re-attribute
	// the PID rather than merely ask whether something holds it.
	serverStart, _ := pidutil.StartTime(serverPID)
	t.Cleanup(func() {
		// The socket may already be unlinked by the sweep under test, so
		// killing by socket path can't be relied on alone -- fall back to a
		// direct PID kill of the exact server this test spawned and
		// verified, never a bare/default-socket kill-server. The socket kill
		// goes through the package's bounded helper: this cleanup runs after
		// a sweep that may have left the server wedged, and an unbounded
		// client call to such a peer stalls the whole package until the outer
		// go-test timeout instead of failing here.
		_ = killTmuxServerAtSocket(socket)
		if pidutil.AliveWithStartTime(serverPID, serverStart) && pidutil.AliveWithCmdline(serverPID, isTmuxArgv) {
			_ = syscall.Kill(serverPID, syscall.SIGKILL)
		}
	})

	if !pidutil.Alive(serverPID) {
		t.Fatalf("tmux server pid %d not alive right after starting it", serverPID)
	}

	// Backdate only now that the server has finished creating its socket
	// file inside dir -- an earlier backdate would be overwritten by that
	// write touching dir's mtime again, leaving it under the sweep's
	// min-age guard (the same ordering TestSweepOrphanPIDPrefixedDirsRemovesFreeSentinel
	// uses relative to HoldAliveSentinel).
	backdatePastSweepAge(t, dir)

	// Recorded before the sweep so a failure can show which process owned the
	// server (and would reap it) while it was known to be alive.
	preSweep := procSnapshot(serverPID)
	var diagnostics bytes.Buffer
	SweepOrphanPIDPrefixedDirs(root, "pfx-", &diagnostics)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("socket parent dir survived sweep, fixture invalid: %s\nsweep diagnostics:\n%s", dir, diagnostics.String())
	}
	if pidutil.AliveWithStartTime(serverPID, serverStart) {
		// Snapshot before anything else runs: the evidence that separates a
		// skipped reap from a liveness probe that misread a dying server is
		// only on the host for as long as the PID's state takes to change.
		postSweep := procSnapshot(serverPID)
		t.Errorf("tmux server pid %d at %s is still alive after its socket parent dir was swept -- orphaned, matches ga-t33q83\n"+
			"captured start token: %q\nsweep diagnostics:\n%s\nbefore sweep:\n%s\nat assertion:\n%s",
			serverPID, socket, serverStart, diagnostics.String(), preSweep, postSweep)
	}
}

// TestTmuxServerIdentityAtSocketNamesWhyNothingAnswered pins the
// classification the sweep skips a reap on: a query nothing answers is
// errNoTmuxServerAtSocket, and it carries tmux's own reason so the sweep's
// diagnostics can tell a stale socket from a live server whose query failed.
func TestTmuxServerIdentityAtSocketNamesWhyNothingAnswered(t *testing.T) {
	RequireTmux(t)

	// Short root for the same sun_path reason as the sweep test above.
	root, err := os.MkdirTemp("/tmp", "tmuxtest-ident-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	socket := filepath.Join(root, "sock")

	_, _, err = tmuxServerIdentityAtSocket(socket)
	if !errors.Is(err, errNoTmuxServerAtSocket) {
		t.Fatalf("tmuxServerIdentityAtSocket(%s) error = %v, want errNoTmuxServerAtSocket", socket, err)
	}
	if !strings.Contains(err.Error(), socket) || !strings.Contains(err.Error(), "exit status") {
		t.Errorf("tmuxServerIdentityAtSocket(%s) error = %q, want the socket path and the client's exit status", socket, err)
	}
}

// procSnapshot renders what the kernel reports about pid right now: the
// signal-0 probe pidutil.Alive starts from, the raw stat row its state and
// start-token reads parse, and the status fields naming state and parent.
// StartTime reaches ps exactly where /proc cannot answer, so its line also
// records what pidutil's ps fallback would have said. While the PID exists on
// Linux the snapshot reads /proc only, so taking it before the sweep does not
// perturb the timing it records.
func procSnapshot(pid int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  kill(%d, 0): %s\n", pid, probeOutcome(syscall.Kill(pid, 0)))
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		fmt.Fprintf(&b, "  /proc/%d/stat: %v\n", pid, err)
	} else {
		fmt.Fprintf(&b, "  /proc/%d/stat: %s\n", pid, strings.TrimSpace(string(stat)))
	}
	status, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		fmt.Fprintf(&b, "  /proc/%d/status: %v\n", pid, err)
	} else {
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "State:") || strings.HasPrefix(line, "PPid:") {
				fmt.Fprintf(&b, "  /proc/%d/status %s\n", pid, line)
			}
		}
	}
	start, err := pidutil.StartTime(pid)
	fmt.Fprintf(&b, "  pidutil.StartTime: %q (%s)\n", start, probeOutcome(err))
	return b.String()
}

// probeOutcome renders a probe's error, or "ok" when it succeeded.
func probeOutcome(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}
