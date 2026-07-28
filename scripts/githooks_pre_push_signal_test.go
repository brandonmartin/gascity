package scripts_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// setMake replaces the fixture's fake `make` so a test can choose how the
// push-time suite ends.
func (f *prePushFixture) setMake(t *testing.T, body string) {
	t.Helper()
	writeExecutable(t, filepath.Join(f.binDir, "make"), body)
}

// pushRefLine is the ref line git feeds the hook for an ordinary update to an
// already-published branch.
func (f *prePushFixture) pushRefLine() string {
	return "refs/heads/main " + f.commitNew + " refs/heads/main " + f.commitOld + "\n"
}

// TestPrePushReportsSignalDeathDistinctly is the ga-8qmy legibility contract.
// The push gate was killed three times by something outside the suite, and
// each time it surfaced as a generic `make` error with zero FAIL lines — so
// it read as a flaky test and the next agent retried into the same wall. A
// gate that dies to a signal produced no verdict at all, and the hook must
// say that in words rather than letting `make: *** Terminated` stand as the
// whole explanation.
func TestPrePushReportsSignalDeathDistinctly(t *testing.T) {
	f := newPrePushFixture(t)
	// Die exactly the way the reaped gate did: SIGTERM to the suite itself.
	f.setMake(t, `#!/usr/bin/env sh
printf '%s\n' "$*" >> "$MAKE_RECORD"
kill -TERM $$
`)

	code, out := f.run(t, f.pushRefLine())

	if code == 0 {
		t.Fatalf("pre-push exit = 0 after a killed gate; an unverified tree must never push\n%s", out)
	}
	if got := f.read(t, f.makeRuns); !strings.Contains(got, "test-fast-parallel") {
		t.Fatalf("push-time suite was never reached (make invocations = %q)", got)
	}
	lower := strings.ToLower(out)
	if !strings.Contains(lower, "kill") {
		t.Fatalf("pre-push output never says the gate was killed:\n%s", out)
	}
	if !strings.Contains(lower, "not a test failure") {
		t.Fatalf("pre-push output does not distinguish a kill from a test failure; the next agent reads it as flake and retries:\n%s", out)
	}
	if !strings.Contains(out, "15") {
		t.Fatalf("pre-push output does not name the signal that killed the gate:\n%s", out)
	}
}

// TestPrePushReportsOrdinaryFailureAsFailure is the other half of the
// contract: a suite that actually ran and failed must keep reading as a
// failure, or the new diagnostic would excuse real regressions.
func TestPrePushReportsOrdinaryFailureAsFailure(t *testing.T) {
	f := newPrePushFixture(t)
	f.setMake(t, `#!/usr/bin/env sh
printf '%s\n' "$*" >> "$MAKE_RECORD"
echo "--- FAIL: TestSomething" >&2
exit 2
`)

	code, out := f.run(t, f.pushRefLine())

	if code == 0 {
		t.Fatalf("pre-push exit = 0 after a failing gate\n%s", out)
	}
	if strings.Contains(strings.ToLower(out), "not a test failure") {
		t.Fatalf("pre-push reported a genuine test failure as a signal kill:\n%s", out)
	}
}

// TestPrePushPropagatesGreenGate keeps the success path intact through the
// change from `exec make` to a waited child: a passing gate must still let
// the push proceed.
func TestPrePushPropagatesGreenGate(t *testing.T) {
	f := newPrePushFixture(t)

	code, out := f.run(t, f.pushRefLine())

	if code != 0 {
		t.Fatalf("pre-push exit = %d for a green gate, want 0\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); !strings.Contains(got, "test-fast-parallel") {
		t.Fatalf("push-time suite was never reached (make invocations = %q)", got)
	}
}
