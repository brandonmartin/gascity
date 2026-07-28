package main

// Bounded managed-dolt server log (ga-fyu0).
//
// The managed `dolt sql-server` appends stdout+stderr to a single dolt.log for
// the life of its generation, and nothing ever reclaimed it: one observed city
// carried a 69MB file spanning many server generations. Size alone would be a
// disk nuisance; the real cost is triage. Since ga-oigp lowered
// `read_timeout_millis` to 15s the server reaps idle per-call connections on
// schedule and logs one ERROR line per reap — by-design behavior at error
// severity, at a volume that reached connection id ~1.1M in 15h. Anyone
// tailing the log to diagnose the alive-but-deaf wedge has to read past that
// flood first.
//
// Rotation is copy-truncate, not rename. Both writers — the direct spawn in
// dolt_start_managed.go and the scope watchdog — hand the server a descriptor
// opened O_APPEND, and the server holds it for its whole generation. Renaming
// the live log would leave the server writing to the detached inode while the
// fresh dolt.log stayed empty until the next restart. Truncating in place
// keeps the inode, and O_APPEND makes the server's next write land at offset 0
// with no sparse hole. The tradeoff is logrotate's: lines written between the
// copy and the truncate are lost. At these sizes that is a handful of lines
// per rotation, against a log that is otherwise unreadable.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// managedDoltLogMaxBytesEnv overrides the size at which dolt.log is
	// rotated. A value of 0 (or negative) disables rotation entirely.
	managedDoltLogMaxBytesEnv = "GC_DOLT_LOG_MAX_BYTES"

	// managedDoltLogKeepEnv overrides how many rotated generations
	// (dolt.log.1 … dolt.log.N) are retained. 0 retains none.
	managedDoltLogKeepEnv = "GC_DOLT_LOG_KEEP"

	// defaultManagedDoltLogMaxBytes bounds the live log at a size a human or
	// agent can actually read. Triage tails the last few hundred lines, so the
	// cap only has to keep the file small enough that `tail`, `grep`, and the
	// diagnostic capture in AGENTS.md stay instant.
	defaultManagedDoltLogMaxBytes int64 = 16 << 20 // 16 MiB

	// defaultManagedDoltLogKeep retains enough history to cover the window an
	// incident is usually noticed in without letting the pack state directory
	// grow without bound: worst case is (keep+1) × cap on disk.
	defaultManagedDoltLogKeep = 3
)

// managedDoltLogMaxBytes resolves the rotation threshold for dolt.log,
// honoring GC_DOLT_LOG_MAX_BYTES. An unset or unparseable value falls back to
// defaultManagedDoltLogMaxBytes; a non-positive value disables rotation.
func managedDoltLogMaxBytes() int64 {
	raw := strings.TrimSpace(os.Getenv(managedDoltLogMaxBytesEnv))
	if raw == "" {
		return defaultManagedDoltLogMaxBytes
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return defaultManagedDoltLogMaxBytes
	}
	if value < 0 {
		return 0
	}
	return value
}

// managedDoltLogKeep resolves how many rotated dolt.log generations to retain,
// honoring GC_DOLT_LOG_KEEP. An unset or unparseable value falls back to
// defaultManagedDoltLogKeep; a negative value retains none.
func managedDoltLogKeep() int {
	raw := strings.TrimSpace(os.Getenv(managedDoltLogKeepEnv))
	if raw == "" {
		return defaultManagedDoltLogKeep
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return defaultManagedDoltLogKeep
	}
	if value < 0 {
		return 0
	}
	return value
}

// managedDoltRotateLogFn is the rotation entry point used by the managed-dolt
// start path and the scope watchdog. Package-level var so tests can observe or
// fail the call without writing multi-megabyte fixtures. Production points at
// rotateManagedDoltLogIfOversized.
var managedDoltRotateLogFn = rotateManagedDoltLogIfOversized

// rotateManagedDoltLogIfOversized applies the environment-resolved size cap
// and retention count to path. It reports whether a rotation happened.
func rotateManagedDoltLogIfOversized(path string) (bool, error) {
	return rotateManagedDoltLog(path, managedDoltLogMaxBytes(), managedDoltLogKeep())
}

// rotateManagedDoltLog copy-truncates path when it exceeds maxBytes, retaining
// up to keep prior generations as path.1 … path.keep (path.1 newest). It
// reports whether a rotation happened.
//
// Rotation is skipped — without error — when maxBytes is non-positive
// (disabled), when path is blank or absent, when path is not a regular file,
// or when the log is still within the cap. A missing log is the normal state
// before the first server start, so it is not a failure.
func rotateManagedDoltLog(path string, maxBytes int64, keep int) (bool, error) {
	path = strings.TrimSpace(path)
	if path == "" || maxBytes <= 0 {
		return false, nil
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat dolt log %q: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() <= maxBytes {
		return false, nil
	}
	if keep < 0 {
		keep = 0
	}
	if keep > 0 {
		if err := shiftManagedDoltLogGenerations(path, keep); err != nil {
			return false, err
		}
		if err := copyManagedDoltLogGeneration(path, path+".1"); err != nil {
			return false, err
		}
	}
	if err := os.Truncate(path, 0); err != nil {
		return false, fmt.Errorf("truncate dolt log %q: %w", path, err)
	}
	return true, nil
}

// shiftManagedDoltLogGenerations ages the retained generations by one, dropping
// the oldest, so path.1 is free for the log about to be rotated. Absent
// generations are expected before the retention window fills and are skipped.
func shiftManagedDoltLogGenerations(path string, keep int) error {
	oldest := managedDoltLogGenerationPath(path, keep)
	if err := os.Remove(oldest); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove oldest dolt log generation %q: %w", oldest, err)
	}
	for i := keep - 1; i >= 1; i-- {
		from := managedDoltLogGenerationPath(path, i)
		to := managedDoltLogGenerationPath(path, i+1)
		if err := os.Rename(from, to); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("age dolt log generation %q to %q: %w", from, to, err)
		}
	}
	return nil
}

// managedDoltLogGenerationPath names the nth retained generation of path.
func managedDoltLogGenerationPath(path string, n int) string {
	return path + "." + strconv.Itoa(n)
}

// copyManagedDoltLogGeneration copies the live log to dst through a temp file
// in the same directory, so a reader never observes a half-written generation
// and a failed copy leaves no partial file behind.
func copyManagedDoltLogGeneration(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open dolt log %q: %w", src, err)
	}
	defer in.Close() //nolint:errcheck // read-only handle

	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create rotation temp for %q: %w", dst, err)
	}
	tmpName := tmp.Name()
	// No-op once the rename below succeeds; the safety net for every path that
	// does not get that far.
	defer os.Remove(tmpName) //nolint:errcheck // best-effort cleanup

	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy dolt log %q to %q: %w", src, dst, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close rotation temp for %q: %w", dst, err)
	}
	// CreateTemp opens 0600; retained generations match the log's own mode so
	// operators can read them the same way.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod rotation temp for %q: %w", dst, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("publish dolt log generation %q: %w", dst, err)
	}
	return nil
}
