package tmuxtest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/testutil"
)

// sweepSocketRoot returns a socket root short enough for a Unix socket path.
// The default temp dir under a test harness can be long enough to blow the
// ~107 byte sun_path limit, which would make the probe server skip rather than
// exercise the sweep.
func sweepSocketRoot(t *testing.T, prefix string) string {
	t.Helper()
	return testutil.ShortTempDir(t, prefix)
}

func TestSweepSocketRootReapsServer(t *testing.T) {
	root := sweepSocketRoot(t, "tmuxsweep-")
	socketPath := StartProbeServer(t, root, "sweep-target")

	if !ProbeServerAlive(socketPath) {
		t.Fatalf("probe server on %s is not alive before the sweep", socketPath)
	}

	if reaped := SweepSocketRoot(root, nil); reaped != 1 {
		t.Fatalf("SweepSocketRoot = %d, want 1", reaped)
	}
	if ProbeServerAlive(socketPath) {
		t.Fatalf("probe server on %s survived the sweep", socketPath)
	}
}

func TestSweepSocketRootReapsServerWhoseSocketDirIsAboutToBeRemoved(t *testing.T) {
	// The regression this guards: the harness deletes the socket root at
	// teardown. Sweeping first must reap the server; sweeping after the
	// removal cannot, because the unlinked socket is unreachable.
	root := sweepSocketRoot(t, "tmuxorder-")
	socketPath := StartProbeServer(t, root, "order-target")

	if reaped := SweepSocketRoot(root, nil); reaped != 1 {
		t.Fatalf("SweepSocketRoot before removal = %d, want 1", reaped)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("RemoveAll(%s): %v", root, err)
	}
	if ProbeServerAlive(socketPath) {
		t.Fatalf("probe server on %s survived a sweep that preceded socket-root removal", socketPath)
	}
}

func TestSweepSocketRootLeavesOtherRootsAlone(t *testing.T) {
	keepRoot := sweepSocketRoot(t, "tmuxkeep-")
	socketPath := StartProbeServer(t, keepRoot, "keep-target")

	sweepRoot := sweepSocketRoot(t, "tmuxother-")
	if reaped := SweepSocketRoot(sweepRoot, nil); reaped != 0 {
		t.Fatalf("sweeping an unrelated root reaped %d server(s), want 0", reaped)
	}
	if !ProbeServerAlive(socketPath) {
		t.Fatalf("probe server on %s was killed by a sweep of a different root", socketPath)
	}
}

func TestSweepSocketRootHandlesEmptyAndMissingRoots(t *testing.T) {
	if reaped := SweepSocketRoot("", nil); reaped != 0 {
		t.Fatalf("empty socket root reaped %d, want 0", reaped)
	}
	if reaped := SweepSocketRoot("   ", nil); reaped != 0 {
		t.Fatalf("blank socket root reaped %d, want 0", reaped)
	}
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if reaped := SweepSocketRoot(missing, nil); reaped != 0 {
		t.Fatalf("missing socket root reaped %d, want 0", reaped)
	}
}

func TestSweepSocketRootSkipsNonSocketEntries(t *testing.T) {
	root := t.TempDir()
	socketDir := filepath.Join(root, fmt.Sprintf("tmux-%d", os.Getuid()))
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", socketDir, err)
	}
	if err := os.WriteFile(filepath.Join(socketDir, "not-a-socket"), []byte("stale"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if reaped := SweepSocketRoot(root, nil); reaped != 0 {
		t.Fatalf("regular file counted as a reaped server: got %d, want 0", reaped)
	}
}

func TestSweepSocketRootReportsReapedServers(t *testing.T) {
	root := sweepSocketRoot(t, "tmuxreport-")
	StartProbeServer(t, root, "report-target")

	var warn strings.Builder
	if reaped := SweepSocketRoot(root, &warn); reaped != 1 {
		t.Fatalf("SweepSocketRoot = %d, want 1", reaped)
	}
	if !strings.Contains(warn.String(), "reaped 1 orphaned tmux server(s)") {
		t.Fatalf("warn output = %q, want a reaped-server diagnostic", warn.String())
	}
	if !strings.Contains(warn.String(), root) {
		t.Fatalf("warn output = %q, want it to name the socket root %s", warn.String(), root)
	}
}

func TestSweepSocketRootStaysQuietWhenNothingReaped(t *testing.T) {
	var warn strings.Builder
	if reaped := SweepSocketRoot(t.TempDir(), &warn); reaped != 0 {
		t.Fatalf("SweepSocketRoot = %d, want 0", reaped)
	}
	if warn.String() != "" {
		t.Fatalf("warn output = %q, want empty when nothing was reaped", warn.String())
	}
}
