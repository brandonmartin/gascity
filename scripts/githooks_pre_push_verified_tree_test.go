package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// treeSHA is the content hash of the commit currently checked out — the
// identity the push gate's dedup is keyed on.
func (f *prePushFixture) treeSHA(t *testing.T) string {
	t.Helper()
	return f.git(t, "rev-parse", "HEAD^{tree}")
}

// markVerified writes the marker the parallel runner would have written after
// a green suite over this exact tree.
func (f *prePushFixture) markVerified(t *testing.T, mode, tree string) string {
	t.Helper()
	dir := filepath.Join(f.repo, ".git", "verified-trees")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	marker := filepath.Join(dir, mode+"."+tree)
	if err := os.WriteFile(marker, []byte("mode="+mode+"\ntree="+tree+"\n"), 0o644); err != nil {
		t.Fatalf("write marker %s: %v", marker, err)
	}
	return marker
}

// goChangeRefLine pushes main forward over the commit that added a Go file, so
// the hook's Go-change scan reaches the suite.
func (f *prePushFixture) goChangeRefLine() string {
	return "refs/heads/main " + f.commitNew + " refs/heads/main " + f.commitOld + "\n"
}

// TestPrePushSkipsSuiteWhenTreeAlreadyVerified is the fix for ga-4rr4. The
// refinery's run-tests step and this hook run the identical fast suite over
// the identical merged tree, so every merge paid for it twice — measured at
// ~12min then ~16min on one branch, roughly half the merge's wall clock spent
// re-deciding a question already answered.
func TestPrePushSkipsSuiteWhenTreeAlreadyVerified(t *testing.T) {
	f := newPrePushFixture(t)
	f.markVerified(t, "fast", f.treeSHA(t))

	code, out := f.run(t, f.goChangeRefLine())
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); got != "" {
		t.Fatalf("push-time suite re-ran a tree already recorded green: %q", got)
	}
	if !strings.Contains(out, "already") {
		t.Fatalf("hook skipped the suite without saying why:\n%s", out)
	}
}

// TestPrePushStillRunsBeadsChainWhenTreeVerified keeps the dedup narrow. Only
// the duplicated test suite is skipped; the bead ownership/staleness guard and
// the beads chain both still have to run, because a mayor ruling that landed
// after this push was queued must still be able to stop it.
func TestPrePushStillRunsBeadsChainWhenTreeVerified(t *testing.T) {
	f := newPrePushFixture(t)
	f.markVerified(t, "fast", f.treeSHA(t))
	refLine := f.goChangeRefLine()

	code, out := f.run(t, refLine)
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if got := f.read(t, f.bdStdin); got != refLine {
		t.Fatalf("beads received stdin %q, want %q; the dedup must not skip the beads chain", got, refLine)
	}
}

// TestPrePushRunsSuiteWhenMarkerIsForAnotherTree pins the property that makes
// this safe to leave on: the marker names one exact content hash, so a tree
// carrying so much as one extra commit is not covered by it.
func TestPrePushRunsSuiteWhenMarkerIsForAnotherTree(t *testing.T) {
	f := newPrePushFixture(t)
	f.markVerified(t, "fast", strings.Repeat("a", 40))

	code, out := f.run(t, f.goChangeRefLine())
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); !strings.Contains(got, "test-fast-parallel") {
		t.Fatalf("push-time suite skipped on a marker belonging to a different tree (make invocations = %q)", got)
	}
}

// TestPrePushRunsSuiteWhenMarkerIsForAnotherSuite stops a cheaper suite from
// vouching for this gate: the hook runs `test-fast-parallel`, so only a `fast`
// verdict answers its question.
func TestPrePushRunsSuiteWhenMarkerIsForAnotherSuite(t *testing.T) {
	f := newPrePushFixture(t)
	f.markVerified(t, "integration", f.treeSHA(t))

	code, out := f.run(t, f.goChangeRefLine())
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); !strings.Contains(got, "test-fast-parallel") {
		t.Fatalf("push-time suite skipped on an `integration` marker (make invocations = %q)", got)
	}
}

// TestPrePushRunsSuiteWhenMarkerExpired bounds how long a verdict speaks. The
// tree hash pins the content but not the toolchain or the host around it.
func TestPrePushRunsSuiteWhenMarkerExpired(t *testing.T) {
	f := newPrePushFixture(t)
	marker := f.markVerified(t, "fast", f.treeSHA(t))
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatalf("backdate marker: %v", err)
	}

	code, out := f.run(t, f.goChangeRefLine())
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); !strings.Contains(got, "test-fast-parallel") {
		t.Fatalf("push-time suite skipped on a 48h-old marker (make invocations = %q)", got)
	}
}

// TestPrePushRunsSuiteWhenWorktreeIsDirty is the correctness case that matters
// most: a marker vouches for HEAD's tree, and uncommitted edits are not in it.
// Honoring the marker here would push content no suite has ever seen.
func TestPrePushRunsSuiteWhenWorktreeIsDirty(t *testing.T) {
	f := newPrePushFixture(t)
	f.markVerified(t, "fast", f.treeSHA(t))
	if err := os.WriteFile(filepath.Join(f.repo, "uncommitted.go"), []byte("package fixture\n"), 0o644); err != nil {
		t.Fatalf("write uncommitted file: %v", err)
	}

	code, out := f.run(t, f.goChangeRefLine())
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); !strings.Contains(got, "test-fast-parallel") {
		t.Fatalf("push-time suite skipped with uncommitted changes present (make invocations = %q)", got)
	}
}

// TestPrePushHonorsIgnoreVerifiedOverride gives an operator chasing a
// suspected flake a way to force the real run without deleting cache files.
func TestPrePushHonorsIgnoreVerifiedOverride(t *testing.T) {
	f := newPrePushFixture(t)
	f.markVerified(t, "fast", f.treeSHA(t))
	f.env = append(f.env, "PUSH_GATE_IGNORE_VERIFIED=1")

	code, out := f.run(t, f.goChangeRefLine())
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); !strings.Contains(got, "test-fast-parallel") {
		t.Fatalf("push-time suite skipped despite PUSH_GATE_IGNORE_VERIFIED=1 (make invocations = %q)", got)
	}
}

// TestPrePushDedupDoesNotWeakenOwnershipGuard keeps the ordering honest: the
// bead guard is what stops a superseded push, and it must still be able to
// reject one whose tree happens to carry a green marker.
func TestPrePushDedupDoesNotWeakenOwnershipGuard(t *testing.T) {
	f := newPrePushFixture(t)
	f.markVerified(t, "fast", f.treeSHA(t))
	writeExecutable(t, filepath.Join(f.repo, "scripts", "push-ownership-guard.sh"), `#!/usr/bin/env bash
assert_bead_still_claimed() { echo "bead no longer claimed by this session" >&2; return 1; }
`)

	code, out := f.run(t, f.goChangeRefLine())
	if code != 1 {
		t.Fatalf("pre-push exit = %d, want 1 (ownership guard must still reject)\n%s", code, out)
	}
}
