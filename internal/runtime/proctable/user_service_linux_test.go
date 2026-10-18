//go:build linux

package proctable

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestForeignUserServiceReadsInjectedCgroup(t *testing.T) {
	root := t.TempDir()
	SetScanRootForTesting(t, root)
	const (
		detachedPID = 5100
		scopePID    = 5200
		ownPID      = 5300
		systemPID   = 5400
	)
	writeCgroup(t, root, detachedPID, "/user.slice/user-1000.slice/user@1000.service/app.slice/run-detached.service")
	writeCgroup(t, root, scopePID, "/user.slice/user-1000.slice/user@1000.service/app.slice/tmux-spawn-abc.scope")
	writeCgroup(t, root, ownPID, "/user.slice/user-1000.slice/user@1000.service/app.slice/agent.service")
	writeCgroup(t, root, systemPID, "/system.slice/rogue.service")

	unit, foreign := ForeignUserService(detachedPID, "")
	if !foreign || unit != "run-detached.service" {
		t.Fatalf("detached service = %q, foreign %v; want run-detached.service, true", unit, foreign)
	}
	unit, foreign = ForeignUserService(detachedPID, "run-detached.service")
	if foreign || unit != "run-detached.service" {
		t.Fatalf("session's own service = %q, foreign %v; want run-detached.service, false", unit, foreign)
	}
	if unit, foreign = ForeignUserService(scopePID, ""); foreign || unit != "" {
		t.Fatalf("scope = %q, foreign %v; want empty, false", unit, foreign)
	}
	if unit, foreign = ForeignUserService(ownPID, "agent.service"); foreign || unit != "agent.service" {
		t.Fatalf("own unit = %q, foreign %v; want agent.service, false", unit, foreign)
	}
	if unit, foreign = ForeignUserService(systemPID, ""); foreign || unit != "" {
		t.Fatalf("system service = %q, foreign %v; want empty, false (not below the user manager)", unit, foreign)
	}
	if unit, foreign = ForeignUserService(99999, ""); foreign || unit != "" {
		t.Fatalf("missing pid = %q, foreign %v; want empty, false", unit, foreign)
	}
}

func writeCgroup(t *testing.T, root string, pid int, path string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte("0::"+path+"\n"), 0o644); err != nil {
		t.Fatalf("write cgroup: %v", err)
	}
}

// TestSystemdRunUserCgroupParsesAsNestedService starts a real transient
// user service and checks the cgroup systemd actually assigns. The orphan
// sweep must not scan live /proc under go test (gastownhall/gascity#2839),
// so this test only reads the service's own cgroup file and stops the unit
// itself. The reap decision against that shape is TestForeignUserServiceReadsInjectedCgroup
// and the sweep/restart tests that drive the fake procfs.
func TestSystemdRunUserCgroupParsesAsNestedService(t *testing.T) {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("systemd-run not on PATH")
	}
	unit := fmt.Sprintf("ga-tdb1n-cgroup-%d", os.Getpid())
	cmd := exec.Command("systemd-run", "--user", "--unit="+unit, "--collect", "--quiet", "sleep", "30")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("systemd-run --user unavailable: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "--user", "stop", unit+".service").Run()
	})

	var mainPID int
	var showErr error
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("systemctl", "--user", "show", "-p", "MainPID", "--value", unit+".service").Output()
		showErr = err
		mainPID, _ = strconv.Atoi(strings.TrimSpace(string(out)))
		if err == nil && mainPID > 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if mainPID <= 1 {
		t.Fatalf("MainPID of %s.service = %d (show: %v); systemd-run did not publish a main process", unit, mainPID, showErr)
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", mainPID))
	if err != nil {
		t.Fatalf("read cgroup of %d: %v", mainPID, err)
	}
	cgroup := cgroupPath(string(data))
	got, ok := UserManagerServiceUnit(cgroup)
	if !ok || got != unit+".service" {
		t.Fatalf("UserManagerServiceUnit(%q) = %q, %v; want %q, true", cgroup, got, ok, unit+".service")
	}

	self, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Fatalf("read self cgroup: %v", err)
	}
	if selfUnit, selfOK := UserManagerServiceUnit(cgroupPath(string(self))); selfOK && selfUnit == got {
		t.Fatalf("test process shares the transient unit %s; the fixture is not a detached service", got)
	}
}

func cgroupPath(text string) string {
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) == 3 && fields[2] != "" {
			return fields[2]
		}
	}
	return ""
}
