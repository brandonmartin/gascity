//go:build linux

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	testDoltFlockHelperEnv      = "GC_TEST_DOLT_FLOCK_HELPER"
	testDoltFlockHelperLockPath = "GC_TEST_DOLT_FLOCK_HELPER_LOCK_PATH"
)

type testDoltFlockHelper struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	output  *bytes.Buffer
	readyAt time.Time
}

func requireProcLocks(t *testing.T) {
	t.Helper()
	f, err := os.Open("/proc/locks")
	if err != nil {
		t.Skipf("/proc/locks unavailable: %v", err)
	}
	_ = f.Close()
}

func startTestDoltFlockHelper(t *testing.T, dir, lockPath string) *testDoltFlockHelper {
	t.Helper()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create helper readiness pipe: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDoltFlockHelperProcess$", "-test.v")
	cmd.Dir = dir
	cmd.Env = sanitizedBaseEnv(
		testDoltFlockHelperEnv+"=1",
		testDoltFlockHelperLockPath+"="+lockPath,
	)
	cmd.ExtraFiles = []*os.File{readyW}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = readyR.Close()
		_ = readyW.Close()
		t.Fatalf("create helper control pipe: %v", err)
	}
	output := &bytes.Buffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = readyR.Close()
		_ = readyW.Close()
		t.Fatalf("start flock helper: %v", err)
	}
	_ = readyW.Close()
	helper := &testDoltFlockHelper{cmd: cmd, stdin: stdin, output: output}
	t.Cleanup(func() {
		_ = helper.stdin.Close()
		if helper.cmd.ProcessState == nil {
			_ = helper.cmd.Process.Kill()
		}
		_ = helper.cmd.Wait()
	})
	ready := make([]byte, 1)
	if _, err := io.ReadFull(readyR, ready); err != nil {
		_ = readyR.Close()
		t.Fatalf("wait for flock helper readiness: %v\n%s", err, output)
	}
	helper.readyAt = time.Now()
	_ = readyR.Close()
	return helper
}

func linuxProcDeviceMajor(dev uint64) uint64 {
	return (dev >> 8 & 0xfff) | (dev >> 32 & 0xfffff000)
}

func linuxProcDeviceMinor(dev uint64) uint64 {
	return (dev & 0xff) | (dev >> 12 & 0xffffff00)
}

func logKnownHeldLockDiagnostics(t *testing.T, helper *testDoltFlockHelper, lockPath string, readyToRead time.Duration) {
	t.Helper()
	pid := helper.cmd.Process.Pid
	t.Logf("helper pid=%d; helper-ready-to-holder-read=%s", pid, readyToRead)

	statusPath := fmt.Sprintf("/proc/%d/status", pid)
	status, err := os.ReadFile(statusPath)
	if err != nil {
		t.Logf("read helper status %s: %v", statusPath, err)
	} else {
		foundNSpid := false
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "NSpid:") {
				t.Logf("helper namespace status %s: %s", statusPath, line)
				foundNSpid = true
				break
			}
		}
		if !foundNSpid {
			t.Logf("helper namespace status %s: NSpid line missing", statusPath)
		}
	}

	lockInfo, err := os.Stat(lockPath)
	if err != nil {
		t.Logf("stat helper lock %s: %v", lockPath, err)
	} else if stat, ok := lockInfo.Sys().(*syscall.Stat_t); !ok {
		t.Logf("helper lock %s has unsupported stat metadata %T", lockPath, lockInfo.Sys())
	} else {
		major := linuxProcDeviceMajor(stat.Dev)
		minor := linuxProcDeviceMinor(stat.Dev)
		t.Logf("helper lock stat dev:inode=%02x:%02x:%d", major, minor, stat.Ino)

		f, err := os.Open("/proc/locks")
		if err != nil {
			t.Logf("read /proc/locks for helper lock diagnostics: %v", err)
		} else {
			rows := 0
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				line := scanner.Text()
				fields := strings.Fields(line)
				if len(fields) < 6 {
					continue
				}
				deviceInode := strings.Split(fields[5], ":")
				if len(deviceInode) != 3 {
					continue
				}
				rowMajor, majorErr := strconv.ParseUint(deviceInode[0], 16, 64)
				rowMinor, minorErr := strconv.ParseUint(deviceInode[1], 16, 64)
				rowInode, inodeErr := strconv.ParseUint(deviceInode[2], 10, 64)
				if majorErr != nil || minorErr != nil || inodeErr != nil || rowInode != stat.Ino {
					continue
				}
				rows++
				deviceMatch := rowMajor == major && rowMinor == minor
				t.Logf("/proc/locks row for helper lock inode (stat device match=%t): %s", deviceMatch, line)
			}
			if err := scanner.Err(); err != nil {
				t.Logf("scan /proc/locks: %v", err)
			}
			if rows == 0 {
				t.Logf("/proc/locks has no FLOCK row for helper lock inode %d", stat.Ino)
			}
			_ = f.Close()
		}

		fdDir := fmt.Sprintf("/proc/%d/fd", pid)
		entries, err := os.ReadDir(fdDir)
		if err != nil {
			t.Logf("read helper descriptors %s: %v", fdDir, err)
		} else {
			foundFD := false
			for _, entry := range entries {
				fdPath := filepath.Join(fdDir, entry.Name())
				fdInfo, err := os.Stat(fdPath)
				if err != nil || !os.SameFile(fdInfo, lockInfo) {
					continue
				}
				foundFD = true
				fdInfoPath := fmt.Sprintf("/proc/%d/fdinfo/%s", pid, entry.Name())
				contents, err := os.ReadFile(fdInfoPath)
				if err != nil {
					t.Logf("read locked helper fdinfo %s: %v", fdInfoPath, err)
					continue
				}
				target, _ := os.Readlink(fdPath)
				t.Logf("helper locked fd=%s target=%q fdinfo:\n%s", entry.Name(), target, contents)
			}
			if !foundFD {
				t.Logf("no helper fd under %s matched lock %s", fdDir, lockPath)
			}
		}
	}
}

func TestDoltFlockHelperProcess(t *testing.T) {
	if os.Getenv(testDoltFlockHelperEnv) != "1" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	lockPath := os.Getenv(testDoltFlockHelperLockPath)
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open helper lock file: %v", err)
	}
	defer f.Close() //nolint:errcheck
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("take helper flock: %v", err)
	}
	ready := os.NewFile(3, "flock-ready")
	if ready == nil {
		t.Fatal("open helper readiness descriptor")
	}
	if _, err := ready.Write([]byte{1}); err != nil {
		t.Fatalf("signal helper readiness: %v", err)
	}
	_ = ready.Close()
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatalf("wait for helper release: %v", err)
	}
}

func TestManagedDoltProcLocksParserUsesHexDeviceAndDecimalInode(t *testing.T) {
	lock, flock, err := parseManagedDoltProcFlock("8: FLOCK ADVISORY WRITE 3675383 103:07:2153249400 0 EOF")
	if err != nil {
		t.Fatalf("parse /proc/locks FLOCK row: %v", err)
	}
	if !flock {
		t.Fatal("FLOCK row was not recognized")
	}
	want := managedDoltProcLock{pid: 3675383, major: 259, minor: 7, inode: 2153249400}
	if lock != want {
		t.Fatalf("parsed lock = %+v, want %+v", lock, want)
	}
}

// writeProcLocksFixture renders a single-row /proc/locks fixture naming pid,
// the given hex device pair, and inode.
func writeProcLocksFixture(t *testing.T, pid int, major, minor uint64, inode uint64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proc-locks")
	row := fmt.Sprintf("1: FLOCK ADVISORY WRITE %d %02x:%02x:%d 0 EOF\n", pid, major, minor, inode)
	if err := os.WriteFile(path, []byte(row), 0o644); err != nil {
		t.Fatalf("write proc locks fixture: %v", err)
	}
	return path
}

func statInode(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %s: unsupported file metadata %T", path, info.Sys())
	}
	return stat.Ino
}

// TestManagedDoltLockHolderPIDsToleratesDivergentSuperblockDevice pins the
// btrfs defect: /proc/locks reports the superblock's s_dev, which on btrfs
// (one anonymous device per subvolume) differs from the st_dev a stat of the
// same file returns. Keying the holder lookup on the device number found no
// rows and silently disabled the SIGKILL gate. The fixture's device column is
// deliberately nonsense; only the inode and the positive fd confirmation may
// decide the match.
func TestManagedDoltLockHolderPIDsToleratesDivergentSuperblockDevice(t *testing.T) {
	requireProcLocks(t)
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	helper := startTestDoltFlockHelper(t, dataDir, lockPath)
	pid := helper.cmd.Process.Pid
	procLocks := writeProcLocksFixture(t, pid, 0xff, 0xfe, statInode(t, lockPath))

	pids, err := managedDoltLockHolderPIDs(lockPath, procLocks)
	if err != nil {
		t.Fatalf("resolve lock holder with divergent device column: %v", err)
	}
	if len(pids) != 1 || pids[0] != pid {
		t.Fatalf("holders = %v, want [%d]; the device column must not gate the match", pids, pid)
	}
}

// TestManagedDoltLockHolderPIDsRejectsInodeCollisionWithoutOpenDescriptor
// covers the other half of inode-only matching: a PID whose FLOCK row carries
// the same inode on a different filesystem does not hold this file, and the
// /proc/<pid>/fd confirmation must exclude it.
func TestManagedDoltLockHolderPIDsRejectsInodeCollisionWithoutOpenDescriptor(t *testing.T) {
	requireProcLocks(t)
	_, lockPath := makeDoltDataDirWithLock(t)
	// This process has the lock file's inode in a FLOCK row per the fixture,
	// but never opened the file, so no descriptor can confirm it.
	procLocks := writeProcLocksFixture(t, os.Getpid(), 0xff, 0xfe, statInode(t, lockPath))

	pids, err := managedDoltLockHolderPIDs(lockPath, procLocks)
	if err != nil {
		t.Fatalf("resolve lock holders: %v", err)
	}
	if len(pids) != 0 {
		t.Fatalf("holders = %v, want none: an inode collision without an open descriptor is not a holder", pids)
	}
}

// TestManagedDoltLockHolderPIDsKeepsUnverifiablePID pins the fail-closed
// direction: when a candidate's descriptor directory cannot be read, the PID
// stays in the holder set so the gate sees a foreign holder and refuses.
func TestManagedDoltLockHolderPIDsKeepsUnverifiablePID(t *testing.T) {
	requireProcLocks(t)
	_, lockPath := makeDoltDataDirWithLock(t)
	procLocks := writeProcLocksFixture(t, 4242, 0xff, 0xfe, statInode(t, lockPath))
	emptyProcRoot := t.TempDir() // no 4242/fd directory to read

	pids, err := managedDoltLockHolderPIDsWithProcRoot(lockPath, procLocks, emptyProcRoot)
	if err != nil {
		t.Fatalf("resolve lock holders: %v", err)
	}
	if len(pids) != 1 || pids[0] != 4242 {
		t.Fatalf("holders = %v, want [4242]: an unverifiable candidate must be kept so the gate fails closed", pids)
	}
}

func TestStopManagedDoltSIGKILLsWedgedSoleLockHolder(t *testing.T) {
	requireProcLocks(t)
	city, lockPath := raceTestCity(t, "[workspace]\nname = \"race-test\"\n\n[daemon]\ndolt_stop_timeout = \"0s\"\n\n[dolt]\ndolt_lock_release_timeout = \"2s\"\n")
	dataDir := filepath.Join(city, ".beads", "dolt")
	helper := startTestDoltFlockHelper(t, dataDir, lockPath)
	pid := helper.cmd.Process.Pid
	if err := os.WriteFile(os.Getenv("GC_DOLT_PID_FILE"), []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	report, err := stopManagedDoltProcessWithOptions(city, "", false)
	if err != nil {
		t.Fatalf("stop-managed refused SIGKILL of its sole lock holder pid %d: %v\n%s", pid, err, helper.output)
	}
	if !report.Forced {
		t.Fatalf("stop-managed did not report forced termination: %+v", report)
	}
	if pidAlive(pid) {
		t.Fatalf("sole lock holder pid %d still alive after stop-managed", pid)
	}
}

func TestManagedDoltLockHolderPIDsSeesKnownHeldLock(t *testing.T) {
	requireProcLocks(t)
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	helper := startTestDoltFlockHelper(t, dataDir, lockPath)

	readAt := time.Now()
	pids, err := managedDoltLockHolderPIDs(lockPath, "/proc/locks")
	readyToRead := readAt.Sub(helper.readyAt)
	if err != nil {
		rereadPIDs, rereadErr := managedDoltLockHolderPIDs(lockPath, "/proc/locks")
		t.Logf("immediate holder re-read = %v, err=%v", rereadPIDs, rereadErr)
		logKnownHeldLockDiagnostics(t, helper, lockPath, readyToRead)
		t.Fatalf("resolve known lock holder: %v", err)
	}
	want := helper.cmd.Process.Pid
	if len(pids) != 1 || pids[0] != want {
		rereadPIDs, rereadErr := managedDoltLockHolderPIDs(lockPath, "/proc/locks")
		t.Logf("immediate holder re-read = %v, err=%v", rereadPIDs, rereadErr)
		logKnownHeldLockDiagnostics(t, helper, lockPath, readyToRead)
		t.Fatalf("holders for known-held lock = %v, want [%d]", pids, want)
	}
}

func TestManagedDoltSIGKILLLockGateRefusesOtherHolder(t *testing.T) {
	requireProcLocks(t)
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	helper := startTestDoltFlockHelper(t, dataDir, lockPath)
	targetPID := os.Getpid()

	err := waitManagedDoltSIGKILLLockGate(targetPID, dataDir, func(int) bool { return true }, time.Second, 0, time.Millisecond)
	if err == nil {
		t.Fatal("expected SIGKILL gate to refuse a lock held by another process")
	}
	if !strings.Contains(err.Error(), "other holder pid(s)") || !strings.Contains(err.Error(), strconv.Itoa(helper.cmd.Process.Pid)) {
		t.Fatalf("expected other-holder reason naming pid %d, got %v", helper.cmd.Process.Pid, err)
	}
	if strings.Contains(err.Error(), "could not measure lock ownership") {
		t.Fatalf("other-holder refusal was misreported as measurement failure: %v", err)
	}
}

// TestManagedDoltSIGKILLLockGateRefusesWhenAnotherDatabaseIsHeld covers a
// data dir holding several databases: the target holds one store lock and a
// stranger holds another. Consulting only the first held lock made the
// sole-holder exception depend on os.ReadDir ordering, so the gate must
// examine every held lock and refuse.
func TestManagedDoltSIGKILLLockGateRefusesWhenAnotherDatabaseIsHeld(t *testing.T) {
	requireProcLocks(t)
	dataDir := t.TempDir()
	makeLock := func(db string) string {
		nomsDir := filepath.Join(dataDir, db, ".dolt", "noms")
		if err := os.MkdirAll(nomsDir, 0o755); err != nil {
			t.Fatalf("mkdir noms dir for %s: %v", db, err)
		}
		lockPath := filepath.Join(nomsDir, "LOCK")
		if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
			t.Fatalf("write lock file for %s: %v", db, err)
		}
		return lockPath
	}
	// "a" sorts first, so os.ReadDir yields the target's lock before the
	// stranger's — the ordering under which the old single-lock gate would
	// have wrongly permitted the kill.
	targetLock := makeLock("a-target-db")
	strangerLock := makeLock("b-stranger-db")
	holdFlock(t, targetLock)
	stranger := startTestDoltFlockHelper(t, dataDir, strangerLock)

	err := waitManagedDoltSIGKILLLockGate(os.Getpid(), dataDir, func(int) bool { return true }, time.Second, 0, time.Millisecond)
	if err == nil {
		t.Fatal("expected SIGKILL gate to refuse while another database's lock is held by a stranger")
	}
	if !strings.Contains(err.Error(), strangerLock) {
		t.Fatalf("expected refusal to name the stranger's lock %q, got %v", strangerLock, err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(stranger.cmd.Process.Pid)) {
		t.Fatalf("expected refusal to name stranger pid %d, got %v", stranger.cmd.Process.Pid, err)
	}
}

func TestManagedDoltSIGKILLLockGateRefusesWhenOwnershipCannotBeMeasured(t *testing.T) {
	requireProcLocks(t)
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	startTestDoltFlockHelper(t, dataDir, lockPath)
	missingProcLocks := filepath.Join(t.TempDir(), "missing-proc-locks")

	err := waitManagedDoltSIGKILLLockGateWithProcLocks(os.Getpid(), dataDir, func(int) bool { return true }, time.Second, 0, time.Millisecond, missingProcLocks)
	if err == nil {
		t.Fatal("expected SIGKILL gate to fail closed when lock ownership cannot be measured")
	}
	if !strings.Contains(err.Error(), "could not measure lock ownership") || !strings.Contains(err.Error(), "read "+missingProcLocks) {
		t.Fatalf("expected could-not-measure reason naming %q, got %v", missingProcLocks, err)
	}
	if strings.Contains(err.Error(), "other holder pid(s)") {
		t.Fatalf("measurement failure was misreported as another holder: %v", err)
	}
}

func TestManagedDoltSIGKILLLockGateRefusesWhenFlockAndProcLocksDisagree(t *testing.T) {
	requireProcLocks(t)
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	startTestDoltFlockHelper(t, dataDir, lockPath)
	emptyProcLocks := filepath.Join(t.TempDir(), "empty-proc-locks")
	if err := os.WriteFile(emptyProcLocks, nil, 0o644); err != nil {
		t.Fatalf("write empty proc locks fixture: %v", err)
	}

	err := waitManagedDoltSIGKILLLockGateWithProcLocks(os.Getpid(), dataDir, func(int) bool { return true }, time.Second, 0, time.Millisecond, emptyProcLocks)
	if err == nil {
		t.Fatal("expected SIGKILL gate to fail closed when flock and proc lock probes disagree")
	}
	if !strings.Contains(err.Error(), "could not measure lock ownership") || !strings.Contains(err.Error(), "found no holder") {
		t.Fatalf("expected empty-holder measurement reason, got %v", err)
	}
}
