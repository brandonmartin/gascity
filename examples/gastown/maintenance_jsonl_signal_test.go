//go:build !windows

package gastown_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestJsonlExportRemovesItsTempFilesWhenTheControllerStopsIt: the controller
// stops an order that runs past its timeout by sending TERM to the process
// group of its shell. The run must exit and leave no temporary file in the
// archive, where the next run's `git add -A` would commit it.
func TestJsonlExportRemovesItsTempFilesWhenTheControllerStopsIt(t *testing.T) {
	// The store stub announces through the FIFO that the export has started,
	// then hangs like a Dolt query that never answers.
	started := filepath.Join(t.TempDir(), "started")
	if err := syscall.Mkfifo(started, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	f := newScopeFixture(t, fmt.Sprintf(`
  *"SELECT * FROM "*"hq"*".issues"*)
    echo started > %q
    exec sleep 60
    ;;`, started))
	f.env["FAKE_SCOPE_DBS"] = "city=hq"
	archive := filepath.Join(f.cityDir, "archive")
	jsonlScopeEnv(t, f, archive)

	outputPath := filepath.Join(t.TempDir(), "output.log")
	outputFile, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outputFile.Close() })
	cmd := exec.Command(coreScriptPath("jsonl-export.sh"))
	cmd.Env = mergeTestEnv(f.env)
	cmd.Stdout, cmd.Stderr = outputFile, outputFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start jsonl-export: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	logOutput := func() string { return f.read(t, outputPath) }
	announced := make(chan error, 1)
	go func() {
		_, err := os.ReadFile(started)
		announced <- err
	}()
	select {
	case err := <-announced:
		if err != nil {
			t.Fatalf("waiting for the export to start: %v\n%s", err, logOutput())
		}
	case err := <-done:
		t.Fatalf("jsonl-export ended before its export started: %v\n%s", err, logOutput())
	case <-time.After(30 * time.Second):
		t.Fatalf("the export never started:\n%s", logOutput())
	}
	inFlight, err := filepath.Glob(filepath.Join(archive, ".jsonl-export.tmp.*", "export.jsonl.tmp.*"))
	if err != nil || len(inFlight) != 1 {
		t.Fatalf("in-flight temporary files = %v (err %v), want exactly one in the run directory\n%s", inFlight, err, logOutput())
	}

	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatalf("send TERM to the process group: %v", err)
	}
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 143 {
			t.Fatalf("jsonl-export ended with %v, want exit status 143\n%s", err, logOutput())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("jsonl-export did not stop after TERM:\n%s", logOutput())
	}

	leftovers, err := filepath.Glob(filepath.Join(archive, ".jsonl-export.tmp.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("the stopped run left %v behind", leftovers)
	}
}
