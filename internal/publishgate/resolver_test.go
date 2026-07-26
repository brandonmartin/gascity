package publishgate

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/git"
)

// gitCmd runs a git command in dir and fails the test on error. Git env is
// stripped so a parent repository (or a pre-commit hook) cannot leak in.
func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = git.SanitizedEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), out, err)
	}
	return strings.TrimSpace(string(out))
}

// newTestRepo builds a repository with two commits on a work branch and
// remote-tracking refs planted by hand, which is what the resolver reads.
// Writing refs/remotes directly avoids needing a second repository to fetch
// from while still exercising the exact ref namespace production uses.
func newTestRepo(t *testing.T) (dir, first, second string) {
	t.Helper()
	dir = t.TempDir()
	gitCmd(t, dir, "init", "--initial-branch=develop")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "config", "user.name", "Test")
	gitCmd(t, dir, "commit", "--allow-empty", "-m", "first")
	first = gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "commit", "--allow-empty", "-m", "second")
	second = gitCmd(t, dir, "rev-parse", "HEAD")
	return dir, first, second
}

func TestGitResolverResolvesRefsAndCountsDistance(t *testing.T) {
	dir, first, second := newTestRepo(t)
	gitCmd(t, dir, "update-ref", "refs/remotes/origin/develop", second)
	gitCmd(t, dir, "update-ref", "refs/remotes/upstream/main", second)
	gitCmd(t, dir, "update-ref", "refs/remotes/origin/polecat/ga-qbq", first)

	r := NewGitResolver(dir)

	for _, ref := range []string{"refs/remotes/origin/develop", "refs/remotes/upstream/main", "HEAD", second} {
		got, err := r.ResolveRef(ref)
		if err != nil {
			t.Fatalf("ResolveRef(%q): %v", ref, err)
		}
		if got != second {
			t.Errorf("ResolveRef(%q) = %s, want %s", ref, got, second)
		}
	}

	behind, err := r.CountBehind(first, second)
	if err != nil {
		t.Fatalf("CountBehind: %v", err)
	}
	if behind != 1 {
		t.Errorf("CountBehind(first, second) = %d, want 1", behind)
	}
	if ahead, err := r.CountBehind(second, first); err != nil || ahead != 0 {
		t.Errorf("CountBehind(second, first) = %d, %v; want 0, nil", ahead, err)
	}
}

// The package-level helpers must read "upstream/main" as that remote's
// branch, not as origin/upstream/main — the ga-b5h target shape.
func TestResolveTargetHonorsRemoteQualifiedNames(t *testing.T) {
	dir, first, second := newTestRepo(t)
	gitCmd(t, dir, "update-ref", "refs/remotes/upstream/main", second)
	gitCmd(t, dir, "update-ref", "refs/remotes/origin/develop", first)
	r := NewGitResolver(dir)

	got, err := ResolveTarget(r, "upstream/main")
	if err != nil {
		t.Fatalf("ResolveTarget(upstream/main): %v", err)
	}
	if got != second {
		t.Errorf("ResolveTarget(upstream/main) = %s, want %s", got, second)
	}

	got, err = ResolveTarget(r, "develop")
	if err != nil {
		t.Fatalf("ResolveTarget(develop): %v", err)
	}
	if got != first {
		t.Errorf("ResolveTarget(develop) = %s, want origin's copy %s", got, first)
	}
}

func TestResolveBranchTipRequiresARemoteRef(t *testing.T) {
	dir, first, _ := newTestRepo(t)
	r := NewGitResolver(dir)

	// The local branch exists but was never pushed: an unpublished branch
	// must not read as publishable.
	if _, err := ResolveBranchTip(r, "develop"); err == nil {
		t.Fatal("ResolveBranchTip(develop) = nil error, want failure for an unpushed branch")
	}

	gitCmd(t, dir, "update-ref", "refs/remotes/origin/develop", first)
	got, err := ResolveBranchTip(r, "develop")
	if err != nil {
		t.Fatalf("ResolveBranchTip after publish: %v", err)
	}
	if got != first {
		t.Errorf("ResolveBranchTip(develop) = %s, want %s", got, first)
	}
}

func TestGitResolverUnresolvedRefIsTyped(t *testing.T) {
	dir, _, _ := newTestRepo(t)
	_, err := NewGitResolver(dir).ResolveRef("refs/remotes/origin/never-existed")
	if !errors.Is(err, ErrUnresolvedRef) {
		t.Fatalf("err = %v, want ErrUnresolvedRef", err)
	}
}

func TestGitResolverCurrentBranch(t *testing.T) {
	dir, _, second := newTestRepo(t)
	r := NewGitResolver(dir)

	got, err := r.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if got != "develop" {
		t.Errorf("CurrentBranch = %q, want develop", got)
	}

	// A detached HEAD is not a branch: stamping paths depend on this being
	// an error rather than an empty string they might compare against.
	gitCmd(t, dir, "checkout", "--detach", second)
	if _, err := r.CurrentBranch(); err == nil {
		t.Error("CurrentBranch on a detached HEAD = nil error, want failure")
	}
}

// Bead metadata is agent-written and must never reach git's argv as an
// option or a multi-token string.
func TestGitResolverRejectsOptionShapedRevisions(t *testing.T) {
	dir, _, _ := newTestRepo(t)
	r := NewGitResolver(dir)

	for _, bad := range []string{"", "   ", "--upload-pack=touch /tmp/pwn", "-n", "develop main"} {
		if _, err := r.ResolveRef(bad); err == nil {
			t.Errorf("ResolveRef(%q) = nil error, want rejection", bad)
		}
		if _, err := r.CountBehind(bad, "HEAD"); err == nil {
			t.Errorf("CountBehind(%q, HEAD) = nil error, want rejection", bad)
		}
		if _, err := r.CountBehind("HEAD", bad); err == nil {
			t.Errorf("CountBehind(HEAD, %q) = nil error, want rejection", bad)
		}
	}
}

func TestResolveHelpersRejectEmptyInput(t *testing.T) {
	dir, _, _ := newTestRepo(t)
	r := NewGitResolver(dir)
	if _, err := ResolveTarget(r, "  "); err == nil {
		t.Error("ResolveTarget(blank) = nil error, want failure")
	}
	if _, err := ResolveBranchTip(r, ""); err == nil {
		t.Error("ResolveBranchTip(blank) = nil error, want failure")
	}
	if _, err := ResolveTarget(nil, "develop"); err == nil {
		t.Error("ResolveTarget(nil resolver) = nil error, want failure")
	}
}
