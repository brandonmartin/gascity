package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEnsureDoltIdentityErrorMessages exercises the ensure_dolt_identity
// helper from examples/bd/assets/scripts/gc-beads-bd.sh against stub `dolt`
// and `git` binaries on PATH. The bug being guarded against: when a user
// has set ONLY `dolt config --global --add user.name`, the previous
// implementation reported "git user.name not available" and told the user
// to set user.name (which they already had). The corrected helper reports
// the field that is actually missing — user.email.
func TestEnsureDoltIdentityErrorMessages(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}

	fnSrc := extractShellFunction(t, gcBeadsBdScriptSource(t), "ensure_dolt_identity")

	type fakeStore struct {
		name  string
		email string
	}
	type wantOutcome struct {
		exitOK             bool
		mustContain        []string
		mustNotContain     []string
		expectDoltNameSet  string
		expectDoltEmailSet string
	}
	cases := []struct {
		name string
		dolt fakeStore
		git  fakeStore
		want wantOutcome
	}{
		{
			name: "dolt_has_both_returns_ok",
			dolt: fakeStore{name: "Roger", email: "roger@example.com"},
			want: wantOutcome{exitOK: true},
		},
		{
			name: "dolt_only_name_git_empty_reports_email_missing_not_name",
			dolt: fakeStore{name: "Roger"},
			want: wantOutcome{
				exitOK:         false,
				mustContain:    []string{"user.email"},
				mustNotContain: []string{`add user.name "Your Name"`},
			},
		},
		{
			name: "dolt_only_email_git_empty_reports_name_missing_not_email",
			dolt: fakeStore{email: "roger@example.com"},
			want: wantOutcome{
				exitOK:         false,
				mustContain:    []string{"user.name"},
				mustNotContain: []string{`add user.email "you@example.com"`},
			},
		},
		{
			name: "dolt_empty_git_empty_reports_both_missing",
			want: wantOutcome{
				exitOK:      false,
				mustContain: []string{"user.name", "user.email"},
			},
		},
		{
			name: "dolt_empty_git_has_both_backfills_dolt",
			git:  fakeStore{name: "Roger", email: "roger@example.com"},
			want: wantOutcome{
				exitOK:             true,
				expectDoltNameSet:  "Roger",
				expectDoltEmailSet: "roger@example.com",
			},
		},
		{
			name: "dolt_name_git_email_backfills_only_email",
			dolt: fakeStore{name: "Roger"},
			git:  fakeStore{email: "roger@example.com"},
			want: wantOutcome{
				exitOK:             true,
				expectDoltEmailSet: "roger@example.com",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			writeFakeDolt(t, binDir, tc.dolt.name, tc.dolt.email)
			writeFakeGit(t, binDir, tc.git.name, tc.git.email)

			doltLog := filepath.Join(binDir, "dolt-set.log")
			origPath := os.Getenv("PATH")

			script := fnSrc + "\n" +
				"die() { printf '%s\\n' \"$*\" >&2; exit 1; }\n" +
				"ensure_dolt_identity\n"

			cmd := exec.Command("bash", "-c", script)
			cmd.Env = append(os.Environ(),
				"PATH="+binDir+string(os.PathListSeparator)+origPath,
				"FAKE_DOLT_LOG="+doltLog,
			)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			runErr := cmd.Run()

			if tc.want.exitOK {
				if runErr != nil {
					t.Fatalf("expected success, got %v\nstderr:\n%s", runErr, stderr.String())
				}
			} else {
				if runErr == nil {
					t.Fatalf("expected non-zero exit, got success\nstderr:\n%s", stderr.String())
				}
			}
			out := stderr.String()
			for _, frag := range tc.want.mustContain {
				if !strings.Contains(out, frag) {
					t.Errorf("stderr missing %q:\n%s", frag, out)
				}
			}
			for _, frag := range tc.want.mustNotContain {
				if strings.Contains(out, frag) {
					t.Errorf("stderr should not contain %q (it is misleading guidance):\n%s", frag, out)
				}
			}
			if tc.want.expectDoltNameSet != "" {
				if !logContains(doltLog, "set user.name "+tc.want.expectDoltNameSet) {
					t.Errorf("expected dolt user.name to be set to %q; log:\n%s",
						tc.want.expectDoltNameSet, readFile(doltLog))
				}
			}
			if tc.want.expectDoltEmailSet != "" {
				if !logContains(doltLog, "set user.email "+tc.want.expectDoltEmailSet) {
					t.Errorf("expected dolt user.email to be set to %q; log:\n%s",
						tc.want.expectDoltEmailSet, readFile(doltLog))
				}
			}
		})
	}
}

// gcBeadsBdScriptSource returns the shipped gc-beads-bd.sh source. Shell
// helpers are tested by extracting them from this text rather than by sourcing
// the script, whose top level parses argv and dispatches an op.
func gcBeadsBdScriptSource(t *testing.T) string {
	t.Helper()
	scriptPath := gcBeadsBdSourcePath(t)
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", scriptPath, err)
	}
	return string(data)
}

// gcBeadsBdSourcePath locates the shipped gc-beads-bd.sh in the repo tree.
func gcBeadsBdSourcePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRootForLint(t), "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
}

func extractShellFunction(t *testing.T, script, name string) string {
	t.Helper()
	// Match the function header and capture lines until the matching
	// closing brace at column 0. The script uses the conventional
	// `name() {` ... `\n}` shape.
	pattern := regexp.MustCompile(`(?ms)^` + regexp.QuoteMeta(name) + `\(\)\s*\{.*?\n\}`)
	loc := pattern.FindStringIndex(script)
	if loc == nil {
		t.Fatalf("could not find shell function %q in script", name)
	}
	return script[loc[0]:loc[1]]
}

func writeFakeDolt(t *testing.T, dir, name, email string) {
	t.Helper()
	body := `#!/usr/bin/env bash
# Stub: only handles "config --global --get|--add user.name|user.email".
set -e
log_file=${FAKE_DOLT_LOG:-/dev/null}
case "$1 $2" in
  "config --global")
    case "$3" in
      --get)
        case "$4" in
          user.name)
` + emitGetIf(name) + `
            ;;
          user.email)
` + emitGetIf(email) + `
            ;;
        esac
        ;;
      --add)
        echo "set $4 $5" >> "$log_file"
        exit 0
        ;;
    esac
    ;;
esac
exit 0
`
	writeExecutable(t, filepath.Join(dir, "dolt"), body)
}

func writeFakeGit(t *testing.T, dir, name, email string) {
	t.Helper()
	body := `#!/usr/bin/env bash
set -e
case "$1 $2" in
  "config --global")
    case "$3" in
      user.name)
` + emitGetIf(name) + `
        ;;
      user.email)
` + emitGetIf(email) + `
        ;;
    esac
    ;;
esac
exit 0
`
	writeExecutable(t, filepath.Join(dir, "git"), body)
}

func emitGetIf(value string) string {
	if value == "" {
		return "            exit 1"
	}
	return "            echo " + value + "; exit 0"
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func logContains(path, want string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), want)
}

func readFile(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

// The gc-beads-bd.sh log-rotation tests live here beside the identity tests
// because both drive a shell helper extracted from that one script, and this
// file owns that harness (gcBeadsBdScriptSource, extractShellFunction,
// writeExecutable). The Go side of the same size cap is covered in
// dolt_log_rotate_test.go.

// extractShellAssignment returns the top-level `name=value` line from script.
// The rotation defaults are plain assignments rather than function bodies, so
// extractShellFunction cannot reach them.
func extractShellAssignment(t *testing.T, script, name string) string {
	t.Helper()
	pattern := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `=.*$`)
	line := pattern.FindString(script)
	if line == "" {
		t.Fatalf("could not find shell assignment %q in script", name)
	}
	return line
}

// runGcBeadsBdRotateLog exercises the real rotate_log_if_oversized from
// gc-beads-bd.sh against logFile. The function and the two default literals it
// reads are extracted from the shipped script rather than restated here, so
// the test cannot drift from the code it guards.
func runGcBeadsBdRotateLog(t *testing.T, logFile string, env ...string) {
	t.Helper()
	script := gcBeadsBdScriptSource(t)
	driver := strings.Join([]string{
		"set -e",
		extractShellAssignment(t, script, "DOLT_LOG_MAX_BYTES_DEFAULT"),
		extractShellAssignment(t, script, "DOLT_LOG_KEEP_DEFAULT"),
		extractShellFunction(t, script, "rotate_log_if_oversized"),
		`LOG_FILE="$1"`,
		"rotate_log_if_oversized",
	}, "\n") + "\n"

	cmd := exec.Command("sh", "-c", driver, "sh", logFile)
	// Scrub the knobs from the inherited environment: an agent session that
	// exports them would otherwise silently override each case's fixture.
	shellEnv := make([]string, 0, len(os.Environ())+len(env))
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "GC_DOLT_LOG_") {
			continue
		}
		shellEnv = append(shellEnv, entry)
	}
	shellEnv = append(shellEnv, env...)
	cmd.Env = shellEnv
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("shell rotation failed: %v\n%s", err, out)
	}
}

// TestShellFallbackRotationBoundsTheLog covers the launch path taken when
// GC_BIN is unset: no Go helper, no scope watchdog, so this per-attempt
// rotation is the only thing bounding accumulation across generations.
func TestShellFallbackRotationBoundsTheLog(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "dolt.log")
	if err := os.WriteFile(logFile, []byte("oversized content\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	if err := os.WriteFile(logFile+".1", []byte("previous generation\n"), 0o644); err != nil {
		t.Fatalf("write generation: %v", err)
	}

	runGcBeadsBdRotateLog(t, logFile, "GC_DOLT_LOG_MAX_BYTES=4", "GC_DOLT_LOG_KEEP=2")

	if data, err := os.ReadFile(logFile); err != nil {
		t.Fatalf("read live log: %v", err)
	} else if len(data) != 0 {
		t.Fatalf("live log = %q, want empty (truncated in place)", data)
	}
	if data, err := os.ReadFile(logFile + ".1"); err != nil {
		t.Fatalf("read generation 1: %v", err)
	} else if string(data) != "oversized content\n" {
		t.Fatalf("generation 1 = %q, want the rotated live log", data)
	}
	if data, err := os.ReadFile(logFile + ".2"); err != nil {
		t.Fatalf("read generation 2: %v", err)
	} else if string(data) != "previous generation\n" {
		t.Fatalf("generation 2 = %q, want the aged generation 1", data)
	}
	if _, err := os.Stat(logFile + ".3"); !os.IsNotExist(err) {
		t.Fatalf("stat generation 3 = %v, want not-exist (keep=2 bounds retention)", err)
	}
}

func TestShellFallbackRotationLeavesLogUnderCapAlone(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "dolt.log")
	if err := os.WriteFile(logFile, []byte("small\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	runGcBeadsBdRotateLog(t, logFile, "GC_DOLT_LOG_MAX_BYTES=1048576")

	if data, err := os.ReadFile(logFile); err != nil {
		t.Fatalf("read live log: %v", err)
	} else if string(data) != "small\n" {
		t.Fatalf("live log = %q, want untouched", data)
	}
	if _, err := os.Stat(logFile + ".1"); !os.IsNotExist(err) {
		t.Fatalf("stat generation 1 = %v, want not-exist", err)
	}
}

// TestShellFallbackRotationHonorsDisableAndKeepZero pins the two off-switches
// to the same meaning the Go resolver gives them.
func TestShellFallbackRotationHonorsDisableAndKeepZero(t *testing.T) {
	for _, disable := range []string{"0", "-1"} {
		t.Run("disabled by "+disable, func(t *testing.T) {
			dir := t.TempDir()
			logFile := filepath.Join(dir, "dolt.log")
			if err := os.WriteFile(logFile, []byte("kept\n"), 0o644); err != nil {
				t.Fatalf("write log: %v", err)
			}
			runGcBeadsBdRotateLog(t, logFile, "GC_DOLT_LOG_MAX_BYTES="+disable)
			if data, err := os.ReadFile(logFile); err != nil {
				t.Fatalf("read live log: %v", err)
			} else if string(data) != "kept\n" {
				t.Fatalf("live log = %q, want untouched (rotation disabled)", data)
			}
		})
	}

	t.Run("keep zero retains no generations", func(t *testing.T) {
		dir := t.TempDir()
		logFile := filepath.Join(dir, "dolt.log")
		if err := os.WriteFile(logFile, []byte("dropped\n"), 0o644); err != nil {
			t.Fatalf("write log: %v", err)
		}
		runGcBeadsBdRotateLog(t, logFile, "GC_DOLT_LOG_MAX_BYTES=1", "GC_DOLT_LOG_KEEP=0")
		if data, err := os.ReadFile(logFile); err != nil {
			t.Fatalf("read live log: %v", err)
		} else if len(data) != 0 {
			t.Fatalf("live log = %q, want empty", data)
		}
		if _, err := os.Stat(logFile + ".1"); !os.IsNotExist(err) {
			t.Fatalf("stat generation 1 = %v, want not-exist", err)
		}
	})
}

// TestShellFallbackRotationPreservesLiveAppendWriter is the shell-side copy of
// the copy-truncate invariant: a server left running from an earlier launch
// attempt holds this log open in append mode.
func TestShellFallbackRotationPreservesLiveAppendWriter(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "dolt.log")
	writer, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open live writer: %v", err)
	}
	defer func() { _ = writer.Close() }()
	if _, err := writer.Write([]byte("pre-rotation output\n")); err != nil {
		t.Fatalf("seed live log: %v", err)
	}

	runGcBeadsBdRotateLog(t, logFile, "GC_DOLT_LOG_MAX_BYTES=1", "GC_DOLT_LOG_KEEP=1")

	if _, err := writer.Write([]byte("post-rotation output\n")); err != nil {
		t.Fatalf("write through live handle after rotation: %v", err)
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read live log: %v", err)
	}
	if string(data) != "post-rotation output\n" {
		t.Fatalf("post-rotation log = %q, want only the post-rotation write", data)
	}
}
