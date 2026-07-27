package tmuxtest

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// probeSessionName is the session StartProbeServer creates so ProbeServerAlive
// has a stable target.
const probeSessionName = "probe"

// SweepSocketRoot kills every tmux server whose socket lives under socketRoot
// and returns the number of servers it reaped. Diagnostics are written to warn
// when non-nil.
//
// A harness that points TMUX_TMPDIR at a private socket root and deletes that
// tree at teardown strands any server still running: removing the tree unlinks
// the socket, and a tmux server with no socket is unreachable by every tmux
// command — `tmux -L <name> kill-server` reports "No such file or directory"
// because no socket exists anywhere — so the process survives until reboot.
// Sweeping while the socket path is still valid is what keeps those servers
// reachable long enough to reap (gastownhall/gascity ga-dsex).
//
// Unlike KillAllTestSessions, which matches the "gctest-" socket names this
// package generates, SweepSocketRoot reaps every socket under the root it is
// given. That covers harnesses whose socket name comes from their own fixture
// (cmd/gc names it after the city under test) without teaching this package
// those names. Only sockets under socketRoot are candidates, so a developer's
// personal tmux server is never reachable.
func SweepSocketRoot(socketRoot string, warn io.Writer) int {
	socketRoot = strings.TrimSpace(socketRoot)
	if socketRoot == "" {
		return 0
	}
	// tmux places sockets at <TMUX_TMPDIR>/tmux-<uid>/<socket-name>.
	matches, err := filepath.Glob(filepath.Join(socketRoot, "tmux-*", "*"))
	if err != nil || len(matches) == 0 {
		return 0
	}
	reaped := 0
	for _, socketPath := range matches {
		info, statErr := os.Stat(socketPath)
		if statErr != nil || info.Mode()&os.ModeSocket == 0 {
			continue
		}
		if killTestSocketPath(socketPath) != nil {
			// A leftover socket file with no live server behind it exits
			// non-zero. That is the common, benign case, so it is neither
			// counted nor reported.
			continue
		}
		reaped++
	}
	if reaped > 0 && warn != nil {
		fmt.Fprintf(warn, "tmuxtest: reaped %d orphaned tmux server(s) under %s\n", reaped, socketRoot) //nolint:errcheck
	}
	return reaped
}

// StartProbeServer starts a detached tmux server on socketRoot under socketName
// and returns the socket path. The server is killed via t.Cleanup, which is
// registered after the caller's socket-root cleanup so the kill runs first and
// the socket is still linked when it does.
//
// It skips the test when tmux is unavailable or the server cannot start, so a
// machine without a usable tmux does not turn a sweep test into a failure.
func StartProbeServer(t testing.TB, socketRoot, socketName string) string {
	t.Helper()
	RequireTmux(t)

	cmd := exec.Command("tmux", tmuxArgs(socketName, "new-session", "-d", "-s", probeSessionName, "sleep", "300")...)
	cmd.Env = probeEnv(socketRoot)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("tmuxtest: starting probe server on %s: %v (%s)", socketRoot, err, strings.TrimSpace(string(out)))
	}

	socketPath := filepath.Join(socketRoot, fmt.Sprintf("tmux-%d", os.Getuid()), socketName)
	t.Cleanup(func() { _ = killTestSocketPath(socketPath) })
	if _, err := os.Stat(socketPath); err != nil {
		t.Skipf("tmuxtest: probe socket %s was not created: %v", socketPath, err)
	}
	return socketPath
}

// ProbeServerAlive reports whether a probe server started by StartProbeServer
// is still reachable on socketPath.
func ProbeServerAlive(socketPath string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxGuardCommandTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "tmux", "-S", socketPath, "has-session", "-t", probeSessionName).Run() == nil
}

// probeEnv returns the environment for a probe server: the caller's, with the
// socket root pointed at socketRoot and any inherited client binding removed.
// Tests here often run inside a tmux session themselves — Gas Town agents run
// that way — and leaving TMUX/TMUX_PANE set would let the probe inherit that
// outer session's context instead of standing alone on socketRoot. Mirrors
// ConfigureProcessEnv, which strips the same variables process-wide.
func probeEnv(socketRoot string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		switch {
		case strings.HasPrefix(entry, tmuxEnv+"="),
			strings.HasPrefix(entry, tmuxPaneEnv+"="),
			strings.HasPrefix(entry, tmuxTmpEnv+"="):
			continue
		}
		env = append(env, entry)
	}
	return append(env, tmuxTmpEnv+"="+socketRoot)
}
