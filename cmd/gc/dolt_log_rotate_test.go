package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeManagedDoltLogFixture writes a dolt.log of exactly size bytes and
// returns its path.
func writeManagedDoltLogFixture(t *testing.T, dir string, size int) string {
	t.Helper()
	path := filepath.Join(dir, "dolt.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o644); err != nil {
		t.Fatalf("write log fixture: %v", err)
	}
	return path
}

func TestRotateManagedDoltLogLeavesLogUnderCapAlone(t *testing.T) {
	dir := t.TempDir()
	path := writeManagedDoltLogFixture(t, dir, 512)

	rotated, err := rotateManagedDoltLog(path, 1024, 3)
	if err != nil {
		t.Fatalf("rotateManagedDoltLog: %v", err)
	}
	if rotated {
		t.Fatal("rotated = true for a log under the cap, want false")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if len(data) != 512 {
		t.Fatalf("log size = %d, want 512 (untouched)", len(data))
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("stat %s.1 = %v, want not-exist (no generation created)", path, err)
	}
}

func TestRotateManagedDoltLogCopyTruncatesOversizedLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dolt.log")
	const content = "line one\nline two\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	rotated, err := rotateManagedDoltLog(path, 4, 3)
	if err != nil {
		t.Fatalf("rotateManagedDoltLog: %v", err)
	}
	if !rotated {
		t.Fatal("rotated = false for a log over the cap, want true")
	}

	live, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read live log: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("live log size = %d, want 0 (truncated in place)", len(live))
	}
	// The live log must survive rotation as a regular file so the server's
	// still-open fd keeps writing to the same inode.
	if info, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("stat live log after rotation: %v", statErr)
	} else if !info.Mode().IsRegular() {
		t.Fatalf("live log mode = %v, want a regular file", info.Mode())
	}

	generation, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read rotated generation: %v", err)
	}
	if string(generation) != content {
		t.Fatalf("rotated generation = %q, want %q", generation, content)
	}
	if info, statErr := os.Stat(path + ".1"); statErr != nil {
		t.Fatalf("stat rotated generation: %v", statErr)
	} else if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("rotated generation mode = %v, want 0644", got)
	}
}

// TestRotateManagedDoltLogPreservesLiveAppendWriter is the load-bearing
// invariant behind copy-truncate: the managed server holds the log open in
// O_APPEND mode for the life of its generation, so truncating the file in
// place must make its next write land at offset 0 rather than re-extending
// the file with a sparse hole. Renaming the live log instead would leave the
// server writing to the detached inode and the fresh dolt.log empty forever.
func TestRotateManagedDoltLogPreservesLiveAppendWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dolt.log")

	writer, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open live writer: %v", err)
	}
	defer func() { _ = writer.Close() }()
	if _, err := writer.Write(bytes.Repeat([]byte("noise\n"), 200)); err != nil {
		t.Fatalf("seed live log: %v", err)
	}

	rotated, err := rotateManagedDoltLog(path, 64, 2)
	if err != nil {
		t.Fatalf("rotateManagedDoltLog: %v", err)
	}
	if !rotated {
		t.Fatal("rotated = false, want true")
	}

	if _, err := writer.Write([]byte("after rotation\n")); err != nil {
		t.Fatalf("write through live handle after rotation: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(data) != "after rotation\n" {
		t.Fatalf("post-rotation log = %q, want %q (append writer must restart at offset 0)", data, "after rotation\n")
	}
}

func TestRotateManagedDoltLogShiftsGenerationsAndDropsOldest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dolt.log")
	for name, content := range map[string]string{
		path:        "current",
		path + ".1": "gen-1",
		path + ".2": "gen-2",
		path + ".3": "gen-3-oldest",
	} {
		if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	rotated, err := rotateManagedDoltLog(path, 1, 3)
	if err != nil {
		t.Fatalf("rotateManagedDoltLog: %v", err)
	}
	if !rotated {
		t.Fatal("rotated = false, want true")
	}

	for name, want := range map[string]string{
		path + ".1": "current",
		path + ".2": "gen-1",
		path + ".3": "gen-2",
	} {
		got, readErr := os.ReadFile(name)
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Fatalf("stat %s.4 = %v, want not-exist (keep=3 bounds the generation count)", path, err)
	}
}

func TestRotateManagedDoltLogKeepZeroDiscardsRotatedContent(t *testing.T) {
	dir := t.TempDir()
	path := writeManagedDoltLogFixture(t, dir, 128)

	rotated, err := rotateManagedDoltLog(path, 1, 0)
	if err != nil {
		t.Fatalf("rotateManagedDoltLog: %v", err)
	}
	if !rotated {
		t.Fatal("rotated = false, want true")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("live log size = %d, want 0", len(data))
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("stat %s.1 = %v, want not-exist (keep=0 retains no generations)", path, err)
	}
}

func TestRotateManagedDoltLogDisabledByNonPositiveCap(t *testing.T) {
	for _, maxBytes := range []int64{0, -1} {
		dir := t.TempDir()
		path := writeManagedDoltLogFixture(t, dir, 4096)

		rotated, err := rotateManagedDoltLog(path, maxBytes, 3)
		if err != nil {
			t.Fatalf("rotateManagedDoltLog(maxBytes=%d): %v", maxBytes, err)
		}
		if rotated {
			t.Fatalf("rotated = true for maxBytes=%d, want false (rotation disabled)", maxBytes)
		}
		if data, readErr := os.ReadFile(path); readErr != nil {
			t.Fatalf("read log: %v", readErr)
		} else if len(data) != 4096 {
			t.Fatalf("log size = %d, want 4096 (untouched)", len(data))
		}
	}
}

func TestRotateManagedDoltLogMissingPathIsNoop(t *testing.T) {
	rotated, err := rotateManagedDoltLog(filepath.Join(t.TempDir(), "absent.log"), 1, 3)
	if err != nil {
		t.Fatalf("rotateManagedDoltLog on missing file: %v", err)
	}
	if rotated {
		t.Fatal("rotated = true for a missing log, want false")
	}

	rotated, err = rotateManagedDoltLog("   ", 1, 3)
	if err != nil {
		t.Fatalf("rotateManagedDoltLog on blank path: %v", err)
	}
	if rotated {
		t.Fatal("rotated = true for a blank path, want false")
	}
}

func TestRotateManagedDoltLogLeavesNoTempFilesBehind(t *testing.T) {
	dir := t.TempDir()
	path := writeManagedDoltLogFixture(t, dir, 256)

	if _, err := rotateManagedDoltLog(path, 1, 2); err != nil {
		t.Fatalf("rotateManagedDoltLog: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("rotation left a temp file behind: %s", entry.Name())
		}
	}
}

func TestManagedDoltLogMaxBytesResolution(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int64
	}{
		{name: "unset uses default", value: "", want: defaultManagedDoltLogMaxBytes},
		{name: "explicit value", value: "1048576", want: 1048576},
		{name: "padded value", value: "  2048  ", want: 2048},
		{name: "zero disables", value: "0", want: 0},
		{name: "negative disables", value: "-5", want: 0},
		{name: "garbage uses default", value: "sixteen", want: defaultManagedDoltLogMaxBytes},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedDoltLogMaxBytesFor(tc.value); got != tc.want {
				t.Fatalf("managedDoltLogMaxBytesFor(%q) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

func TestManagedDoltLogKeepResolution(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int
	}{
		{name: "unset uses default", value: "", want: defaultManagedDoltLogKeep},
		{name: "explicit value", value: "5", want: 5},
		{name: "zero keeps none", value: "0", want: 0},
		{name: "negative keeps none", value: "-2", want: 0},
		{name: "garbage uses default", value: "three", want: defaultManagedDoltLogKeep},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedDoltLogKeepFor(tc.value); got != tc.want {
				t.Fatalf("managedDoltLogKeepFor(%q) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

// rotateManagedDoltLogIfOversized is a one-line composition of the two
// resolvers above and rotateManagedDoltLog, all three exhaustively covered
// here. Its wiring — that GC_DOLT_LOG_MAX_BYTES and GC_DOLT_LOG_KEEP reach the
// right parameters — is covered end to end, in a real process, by
// TestManagedDoltScopeWatchdogRotatesOversizedLog.

// TestStartManagedDoltRotatesLogBeforeSpawn pins the start-path half of the
// wiring: the size cap is applied before the generation begins appending, and
// before the log-offset snapshot that anchors the startup-output read.
func TestStartManagedDoltRotatesLogBeforeSpawn(t *testing.T) {
	city, _ := raceTestCity(t, "")
	if err := os.Remove(filepath.Join(city, ".beads", "dolt", "dolt", ".dolt", "noms", "LOCK")); err != nil {
		t.Fatalf("clear store lock: %v", err)
	}
	shimLockReleaseTimeout(t, 150*time.Millisecond)

	origRotate := managedDoltRotateLogFn
	t.Cleanup(func() { managedDoltRotateLogFn = origRotate })
	var rotatedPaths []string
	managedDoltRotateLogFn = func(path string) (bool, error) {
		rotatedPaths = append(rotatedPaths, path)
		return false, nil
	}

	origStart := managedDoltStartSQLServerFn
	t.Cleanup(func() { managedDoltStartSQLServerFn = origStart })
	sentinel := errors.New("spawn reached")
	rotatedBeforeSpawn := false
	managedDoltStartSQLServerFn = func(string, string, string, *os.File) (managedDoltStartedProcess, error) {
		rotatedBeforeSpawn = len(rotatedPaths) > 0
		return managedDoltStartedProcess{}, sentinel
	}

	if _, err := startManagedDoltProcessWithOptions(city, "127.0.0.1", "13319", "root", "warning", -1, 2*time.Second, false); !errors.Is(err, sentinel) {
		t.Fatalf("expected the stubbed spawn error, got %v", err)
	}
	if !rotatedBeforeSpawn {
		t.Fatal("start spawned dolt sql-server without first applying the log size cap")
	}
	wantLog := filepath.Join(city, ".gc", "runtime", "packs", "dolt", "dolt.log")
	if rotatedPaths[0] != wantLog {
		t.Fatalf("rotated %q, want the layout log file %q", rotatedPaths[0], wantLog)
	}
}

// TestStartManagedDoltSurvivesLogRotationFailure keeps log hygiene subordinate
// to the data plane: a rotation that cannot run is reported, never fatal.
func TestStartManagedDoltSurvivesLogRotationFailure(t *testing.T) {
	city, _ := raceTestCity(t, "")
	if err := os.Remove(filepath.Join(city, ".beads", "dolt", "dolt", ".dolt", "noms", "LOCK")); err != nil {
		t.Fatalf("clear store lock: %v", err)
	}
	shimLockReleaseTimeout(t, 150*time.Millisecond)

	origRotate := managedDoltRotateLogFn
	t.Cleanup(func() { managedDoltRotateLogFn = origRotate })
	managedDoltRotateLogFn = func(string) (bool, error) {
		return false, errors.New("rotation exploded")
	}

	origStart := managedDoltStartSQLServerFn
	t.Cleanup(func() { managedDoltStartSQLServerFn = origStart })
	sentinel := errors.New("spawn reached")
	spawned := false
	managedDoltStartSQLServerFn = func(string, string, string, *os.File) (managedDoltStartedProcess, error) {
		spawned = true
		return managedDoltStartedProcess{}, sentinel
	}

	_, err := startManagedDoltProcessWithOptions(city, "127.0.0.1", "13320", "root", "warning", -1, 2*time.Second, false)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the stubbed spawn error, got %v", err)
	}
	if !spawned {
		t.Fatal("a failed log rotation blocked the managed dolt start")
	}
}
