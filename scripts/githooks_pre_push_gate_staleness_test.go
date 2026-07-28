package scripts_test

import (
	"strings"
	"testing"
)

// advanceGatePolicyOnDevelop publishes a commit on refs/remotes/origin/develop
// that edits gate machinery this checkout does not carry — the exact shape of a
// branch cut before a hook fix landed. The working tree is restored to `main`
// afterwards so the hook under test is still the real one.
func (f *prePushFixture) advanceGatePolicyOnDevelop(t *testing.T, rel string) {
	t.Helper()
	f.git(t, "checkout", "-q", "-b", "gate-policy-ahead")
	sha := f.commitFile(t, rel, "# newer gate machinery\n")
	f.git(t, "update-ref", "refs/remotes/origin/develop", sha)
	f.git(t, "checkout", "-q", "main")
	f.git(t, "branch", "-D", "gate-policy-ahead")
}

// TestPrePushWarnsWhenGatePolicyIsStale is the fix for ga-igiu. git runs hooks
// from the worktree being pushed FROM, never from the branch being pushed TO,
// so a gate fix merged to the integration branch is inert on every branch cut
// before it. The branches that suffer most from a slow gate are the long-lived
// ones, which are also the least likely to have rebased — and they got no
// signal at all connecting their cost to a fix that already exists. One polecat
// burned over an hour on repeated failing pushes, read the cost as a flaky
// test, and parked with `--no-verify` staged at its prompt.
func TestPrePushWarnsWhenGatePolicyIsStale(t *testing.T) {
	f := newPrePushFixture(t)
	f.advanceGatePolicyOnDevelop(t, ".githooks/gate-note.sh")

	code, out := f.run(t, f.pushRefLine())
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0; the notice is advisory and must not block a push\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); !strings.Contains(got, "test-fast-parallel") {
		t.Fatalf("push-time suite never ran (make invocations = %q); the notice explains the cost, it does not replace the gate", got)
	}
	if !strings.Contains(out, "1 commit behind") {
		t.Fatalf("stale-gate notice does not say how far behind this checkout is:\n%s", out)
	}
	if !strings.Contains(out, "origin/develop") {
		t.Fatalf("stale-gate notice does not name the branch carrying the newer gate:\n%s", out)
	}
	if !strings.Contains(out, "rebase") {
		t.Fatalf("stale-gate notice states the problem but gives no remedy:\n%s", out)
	}
}

// TestPrePushGateStalenessCoversSourcedGateLibraries keeps the staleness window
// wider than `.githooks/` alone. The dedup that skips an entire duplicate suite
// (ga-4rr4) shipped in `scripts/lib/verified-tree.sh`, so a checkout current on
// the hook body but stale on the libraries it sources still pays the old cost.
func TestPrePushGateStalenessCoversSourcedGateLibraries(t *testing.T) {
	f := newPrePushFixture(t)
	f.advanceGatePolicyOnDevelop(t, "scripts/lib/gate-note.sh")

	code, out := f.run(t, f.pushRefLine())
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "1 commit behind") {
		t.Fatalf("gate libraries under scripts/lib are not measured for staleness:\n%s", out)
	}
}

// TestPrePushStaleGateNoticeWarnsAgainstNoVerify pins the half of ga-igiu that
// the bead calls out separately: to an agent staring at a gate it believes is
// broken, `--no-verify` looks like the obvious escape hatch. It is the worst
// one available — it disables the whole hook, not merely the slow part.
func TestPrePushStaleGateNoticeWarnsAgainstNoVerify(t *testing.T) {
	f := newPrePushFixture(t)
	f.advanceGatePolicyOnDevelop(t, ".githooks/gate-note.sh")

	code, out := f.run(t, f.pushRefLine())
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "--no-verify") {
		t.Fatalf("stale-gate notice never mentions --no-verify, the escape hatch it exists to pre-empt:\n%s", out)
	}
	if !strings.Contains(out, "ownership guard") {
		t.Fatalf("stale-gate notice does not say what --no-verify actually disables:\n%s", out)
	}
}

// TestPrePushMeasuresStalenessAgainstTheIntegrationBranchNotRemoteHEAD pins
// which branch gets to define current gate policy. `refs/remotes/origin/HEAD`
// is GitHub's default-branch pointer, and on a fork it points at `main` — a
// pristine mirror of upstream that deliberately receives no fork work. Gate
// fixes land on `develop`, measured at 8 commits ahead of `main` on gate paths
// in this repo. Trusting remote HEAD would understate the gap and, worse, print
// `git rebase origin/main` as the remedy, which is forbidden here.
func TestPrePushMeasuresStalenessAgainstTheIntegrationBranchNotRemoteHEAD(t *testing.T) {
	f := newPrePushFixture(t)
	f.advanceGatePolicyOnDevelop(t, ".githooks/gate-note.sh")
	// origin/main carries none of it, and is what remote HEAD points at.
	f.remoteBranch(t, "main", f.commitNew)
	f.git(t, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	code, out := f.run(t, f.pushRefLine())
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "1 commit behind origin/develop") {
		t.Fatalf("staleness was measured against remote HEAD instead of the integration branch carrying the gate fixes:\n%s", out)
	}
	if strings.Contains(out, "rebase origin/main") {
		t.Fatalf("notice tells the pusher to rebase onto origin/main, which carries no fork work:\n%s", out)
	}
}

// TestPrePushFallsBackToRemoteHEADWhenNoIntegrationBranchExists keeps the
// preference above from becoming a hard requirement: a repo naming its trunk
// something other than develop/main/master still gets a measurement.
func TestPrePushFallsBackToRemoteHEADWhenNoIntegrationBranchExists(t *testing.T) {
	f := newPrePushFixture(t)
	f.git(t, "checkout", "-q", "-b", "trunk-ahead")
	sha := f.commitFile(t, ".githooks/gate-note.sh", "# newer gate machinery\n")
	f.git(t, "update-ref", "refs/remotes/origin/trunk", sha)
	f.git(t, "checkout", "-q", "main")
	f.git(t, "branch", "-D", "trunk-ahead")
	f.git(t, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")

	code, out := f.run(t, f.pushRefLine())
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "1 commit behind origin/trunk") {
		t.Fatalf("no integration branch matched and remote HEAD was not consulted as the fallback:\n%s", out)
	}
}

// TestPrePushSaysNothingWhenGatePolicyIsCurrent keeps the notice from becoming
// wallpaper. A rebased branch runs the current gate and has nothing to be told.
func TestPrePushSaysNothingWhenGatePolicyIsCurrent(t *testing.T) {
	f := newPrePushFixture(t)
	f.remoteBranch(t, "develop", f.commitNew)

	code, out := f.run(t, f.pushRefLine())
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if strings.Contains(out, "behind") {
		t.Fatalf("pre-push reported a stale gate on a checkout carrying the current one:\n%s", out)
	}
	if strings.Contains(out, "--no-verify") {
		t.Fatalf("pre-push volunteered --no-verify guidance on a clean, passing push:\n%s", out)
	}
}

// TestPrePushStaleGateNoticeOnlyFiresWhenTheSuiteRuns ties the notice to the
// cost it explains. A push that changes no Go sources skips the suite outright,
// so a stale gate charged it nothing and there is no tax to account for.
func TestPrePushStaleGateNoticeOnlyFiresWhenTheSuiteRuns(t *testing.T) {
	f := newPrePushFixture(t)
	f.advanceGatePolicyOnDevelop(t, ".githooks/gate-note.sh")
	head := f.commitFile(t, "NOTES.md", "docs only\n")
	refLine := "refs/heads/main " + head + " refs/heads/main " + f.commitNew + "\n"

	code, out := f.run(t, refLine)
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); got != "" {
		t.Fatalf("push-time suite ran for a docs-only push: %q", got)
	}
	if strings.Contains(out, "behind") {
		t.Fatalf("stale-gate notice fired on a push that paid nothing for the stale gate:\n%s", out)
	}
}

// TestPrePushWarnsAgainstNoVerifyWhenGateFails covers the other moment an agent
// reaches for the bypass: a red gate it has decided is not its fault. A failing
// suite is exactly when the hook must be clear that skipping it publishes
// unverified work rather than fixing anything.
func TestPrePushWarnsAgainstNoVerifyWhenGateFails(t *testing.T) {
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
	if !strings.Contains(out, "--no-verify") {
		t.Fatalf("a failing gate gives no warning about --no-verify:\n%s", out)
	}
}

// TestPrePushWarnsAgainstNoVerifyWhenGateIsKilled is the reaped-gate case from
// ga-8qmy read through ga-igiu's lens: a gate killed mid-run produces no FAIL
// lines, which reads as flake, which is precisely the state that ends in
// `--no-verify`. The kill diagnostic and the bypass warning belong together.
func TestPrePushWarnsAgainstNoVerifyWhenGateIsKilled(t *testing.T) {
	f := newPrePushFixture(t)
	f.setMake(t, `#!/usr/bin/env sh
printf '%s\n' "$*" >> "$MAKE_RECORD"
kill -TERM $$
`)

	code, out := f.run(t, f.pushRefLine())
	if code == 0 {
		t.Fatalf("pre-push exit = 0 after a killed gate\n%s", out)
	}
	if !strings.Contains(out, "--no-verify") {
		t.Fatalf("a killed gate gives no warning about --no-verify:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "not a test failure") {
		t.Fatalf("the bypass warning displaced the kill diagnostic; both must survive:\n%s", out)
	}
}
