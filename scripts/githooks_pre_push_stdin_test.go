package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// prePushFixture is a throwaway git repo wired to run the real .githooks/pre-push
// against fake `bd` and `make` binaries.
type prePushFixture struct {
	repo      string
	binDir    string
	bdStdin   string
	makeRuns  string
	commitOld string
	commitNew string
	env       []string
}

// newPrePushFixture builds a repo with two commits — the second adds a Go file —
// so the hook's `git diff -- '*.go'` scan has something real to resolve.
func newPrePushFixture(t *testing.T) *prePushFixture {
	t.Helper()
	root := repoRoot(t)
	repo := t.TempDir()
	binDir := t.TempDir()
	recordDir := t.TempDir()

	f := &prePushFixture{
		repo:     repo,
		binDir:   binDir,
		bdStdin:  filepath.Join(recordDir, "bd-stdin"),
		makeRuns: filepath.Join(recordDir, "make-runs"),
	}
	f.env = append(os.Environ(),
		"PATH="+binDir+":/usr/bin:/bin",
		"GIT_CONFIG_GLOBAL="+filepath.Join(recordDir, "gitconfig"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(recordDir, "gitconfig-system"),
		"BD_STDIN_RECORD="+f.bdStdin,
		"MAKE_RECORD="+f.makeRuns,
	)

	// `bd` records exactly what the chain handed it on stdin.
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env sh
cat > "$BD_STDIN_RECORD"
`)
	// `make` records that the push-time suite was reached.
	writeExecutable(t, filepath.Join(binDir, "make"), `#!/usr/bin/env sh
printf '%s\n' "$*" >> "$MAKE_RECORD"
`)

	for _, dir := range []string{".githooks/lib", "scripts"} {
		if err := os.MkdirAll(filepath.Join(repo, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	for _, rel := range []string{".githooks/pre-push", ".githooks/lib/beads-chain.sh"} {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		writeExecutable(t, filepath.Join(repo, rel), string(body))
	}
	// The ownership guard talks to a live bead store; stub it so this test
	// isolates the hook's stdin composition.
	writeExecutable(t, filepath.Join(repo, "scripts", "push-ownership-guard.sh"), `#!/usr/bin/env bash
assert_bead_still_claimed() { return 0; }
`)

	f.git(t, "init", "-q", "-b", "main")
	f.git(t, "config", "user.email", "test@example.com")
	f.git(t, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	f.git(t, "add", "-A")
	f.git(t, "commit", "-q", "--no-verify", "-m", "base")
	f.commitOld = f.git(t, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(repo, "touched.go"), []byte("package fixture\n"), 0o644); err != nil {
		t.Fatalf("write go file: %v", err)
	}
	f.git(t, "add", "-A")
	f.git(t, "commit", "-q", "--no-verify", "-m", "add go file")
	f.commitNew = f.git(t, "rev-parse", "HEAD")

	return f
}

// git runs a git command in the fixture repo and returns its trimmed output.
// Callers that only need the side effect ignore the result.
func (f *prePushFixture) git(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = f.repo
	cmd.Env = f.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// run invokes the fixture's pre-push hook with the given ref lines on stdin.
func (f *prePushFixture) run(t *testing.T, stdin string) (int, string) {
	t.Helper()
	cmd := exec.Command(filepath.Join(f.repo, ".githooks", "pre-push"), "origin", "git@example.com:fixture.git")
	cmd.Dir = f.repo
	cmd.Env = f.env
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if !asExitError(err, &exitErr) {
		t.Fatalf("run pre-push: %v\n%s", err, out)
	}
	return exitErr.ExitCode(), string(out)
}

// remoteBranch registers a remote-tracking ref, mirroring the
// refs/remotes/origin/* a real clone carries. The hook's new-branch base
// resolution diffs against these.
func (f *prePushFixture) remoteBranch(t *testing.T, name, sha string) {
	t.Helper()
	f.git(t, "update-ref", "refs/remotes/origin/"+name, sha)
}

// commitFile commits one file on the current branch and returns the new sha.
func (f *prePushFixture) commitFile(t *testing.T, name, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.repo, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	f.git(t, "add", "-A")
	f.git(t, "commit", "-q", "--no-verify", "-m", "add "+name)
	return f.git(t, "rev-parse", "HEAD")
}

func (f *prePushFixture) read(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// TestPrePushReplaysRefListToBeadsAndSuiteScan pins the composition introduced
// with the beads chain: git delivers the ref list on stdin exactly once, and
// both consumers — the beads pre-push hook and the Go-change scan that gates
// the push-time suite — must see it. Reading it in only one place silently
// disables the other.
func TestPrePushReplaysRefListToBeadsAndSuiteScan(t *testing.T) {
	f := newPrePushFixture(t)
	refLine := "refs/heads/main " + f.commitNew + " refs/heads/main " + f.commitOld + "\n"

	code, out := f.run(t, refLine)
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}

	if got := f.read(t, f.bdStdin); got != refLine {
		t.Fatalf("beads received stdin %q, want %q", got, refLine)
	}
	if got := f.read(t, f.makeRuns); !strings.Contains(got, "test-fast-parallel") {
		t.Fatalf("push-time suite not reached (make invocations = %q); the Go-change scan lost its stdin", got)
	}
}

// TestPrePushSkipsSuiteForBranchDeletion keeps the pre-existing carve-out
// intact through the stdin rework: a deletion has nothing to test, but beads
// must still see the ref line.
func TestPrePushSkipsSuiteForBranchDeletion(t *testing.T) {
	f := newPrePushFixture(t)
	zero := strings.Repeat("0", 40)
	refLine := "(delete) " + zero + " refs/heads/gone " + f.commitOld + "\n"

	code, out := f.run(t, refLine)
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}

	if got := f.read(t, f.bdStdin); got != refLine {
		t.Fatalf("beads received stdin %q, want %q", got, refLine)
	}
	if got := f.read(t, f.makeRuns); got != "" {
		t.Fatalf("push-time suite ran for a branch deletion: %q", got)
	}
}

// TestPrePushHandlesEmptyRefList guards the emptiness check around the replay:
// re-terminating an empty buffer would manufacture a blank ref line, which the
// scan would read as a real non-deletion push.
func TestPrePushHandlesEmptyRefList(t *testing.T) {
	f := newPrePushFixture(t)

	code, out := f.run(t, "")
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0 for an empty ref list\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); got != "" {
		t.Fatalf("push-time suite ran with no refs pushed: %q", got)
	}
}

// TestPrePushSkipsSuiteForNewBranchWithoutGoChanges pins the fix for ga-vlhp:
// a zero remote sha means "branch is new on the remote", not "no base exists".
// Treating it as the latter ran the ~30min suite on every first push — including
// branches touching zero Go files — which is every polecat's first push by
// definition, and longer than a polecat's per-tool timeout.
func TestPrePushSkipsSuiteForNewBranchWithoutGoChanges(t *testing.T) {
	f := newPrePushFixture(t)
	// origin already carries everything on main, touched.go included.
	f.remoteBranch(t, "main", f.commitNew)

	f.git(t, "checkout", "-q", "-b", "docs-only")
	head := f.commitFile(t, "NOTES.md", "docs only\n")
	zero := strings.Repeat("0", 40)
	refLine := "refs/heads/docs-only " + head + " refs/heads/docs-only " + zero + "\n"

	code, out := f.run(t, refLine)
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); got != "" {
		t.Fatalf("push-time suite ran for a first push touching no Go files: %q", got)
	}
	if got := f.read(t, f.bdStdin); got != refLine {
		t.Fatalf("beads received stdin %q, want %q", got, refLine)
	}
}

// TestPrePushRunsSuiteForNewBranchWithGoChanges is the other half of the
// ga-vlhp fix: cheapening the first-push path must not weaken the gate. A new
// branch whose commits touch Go sources still runs the suite.
func TestPrePushRunsSuiteForNewBranchWithGoChanges(t *testing.T) {
	f := newPrePushFixture(t)
	f.remoteBranch(t, "main", f.commitNew)

	f.git(t, "checkout", "-q", "-b", "go-change")
	head := f.commitFile(t, "added.go", "package fixture\n\nfunc Added() {}\n")
	zero := strings.Repeat("0", 40)
	refLine := "refs/heads/go-change " + head + " refs/heads/go-change " + zero + "\n"

	code, out := f.run(t, refLine)
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); !strings.Contains(got, "test-fast-parallel") {
		t.Fatalf("push-time suite skipped for a first push that changes Go sources (make invocations = %q)", got)
	}
}

// TestPrePushRunsSuiteWhenNewBranchHasNoResolvableBase keeps the fallback
// conservative: with no remote-tracking ref to diff against we cannot tell what
// the branch adds, so the gate must run the suite rather than assume it is
// clean.
func TestPrePushRunsSuiteWhenNewBranchHasNoResolvableBase(t *testing.T) {
	f := newPrePushFixture(t)
	// Deliberately no refs/remotes/* in this fixture.
	f.git(t, "checkout", "-q", "-b", "unmoored")
	head := f.commitFile(t, "NOTES.md", "docs only\n")
	zero := strings.Repeat("0", 40)
	refLine := "refs/heads/unmoored " + head + " refs/heads/unmoored " + zero + "\n"

	code, out := f.run(t, refLine)
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); !strings.Contains(got, "test-fast-parallel") {
		t.Fatalf("push-time suite skipped with no resolvable base (make invocations = %q); the fallback must stay conservative", got)
	}
}

// TestPrePushPrefersTightestNewBranchBase pins base selection when several
// integration branches are candidates: the newest common ancestor wins, so the
// diff covers exactly what this branch adds. A looser base would re-scan
// already-published commits and re-run the suite for Go changes that were
// gated when they landed.
func TestPrePushPrefersTightestNewBranchBase(t *testing.T) {
	f := newPrePushFixture(t)
	// main lags at the pre-Go-file commit; develop carries touched.go.
	f.remoteBranch(t, "main", f.commitOld)
	f.remoteBranch(t, "develop", f.commitNew)

	f.git(t, "checkout", "-q", "-b", "docs-off-develop", f.commitNew)
	head := f.commitFile(t, "NOTES.md", "docs only\n")
	zero := strings.Repeat("0", 40)
	refLine := "refs/heads/docs-off-develop " + head + " refs/heads/docs-off-develop " + zero + "\n"

	code, out := f.run(t, refLine)
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); got != "" {
		t.Fatalf("push-time suite ran using a looser base than origin/develop: %q", got)
	}
}

// TestPrePushPropagatesBeadsRejection keeps beads authoritative at push time:
// its rejection must abort before the suite runs.
func TestPrePushPropagatesBeadsRejection(t *testing.T) {
	f := newPrePushFixture(t)
	writeExecutable(t, filepath.Join(f.binDir, "bd"), `#!/usr/bin/env sh
cat > "$BD_STDIN_RECORD"
echo "beads rejected this push" >&2
exit 9
`)
	refLine := "refs/heads/main " + f.commitNew + " refs/heads/main " + f.commitOld + "\n"

	code, out := f.run(t, refLine)
	if code != 9 {
		t.Fatalf("pre-push exit = %d, want 9 (beads rejection must abort the push)\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); got != "" {
		t.Fatalf("push-time suite ran after a beads rejection: %q", got)
	}
}

// wantSSHKeepalive is the keepalive command this hook asserts. It must match
// what a long-running push-time suite needs to outlast GitHub's idle-socket
// timeout (ga-7i1o): git opens the SSH transport before this hook runs and
// reuses it for the pack write after the hook exits, so a gate that runs long
// enough leaves that socket idle until the far end closes it.
const wantSSHKeepalive = "ssh -o ServerAliveInterval=20 -o ServerAliveCountMax=180 -o TCPKeepAlive=yes"

// TestPrePushSetsSshKeepaliveWhenUnset pins the fix for ga-7i1o: a push whose
// gate runs long enough leaves the SSH transport idle until GitHub closes it,
// so the pack write lands on a dead socket (exit 141) with the ref never
// moving, even though the gate itself passed. core.sshCommand keepalives
// prevent that; this hook must assert them itself so a worktree that never
// ran `make setup` is still covered on its next push.
func TestPrePushSetsSshKeepaliveWhenUnset(t *testing.T) {
	f := newPrePushFixture(t)

	code, out := f.run(t, "")
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}

	got := f.git(t, "config", "--get", "core.sshCommand")
	if got != wantSSHKeepalive {
		t.Fatalf("core.sshCommand = %q, want %q", got, wantSSHKeepalive)
	}
}

// TestPrePushPreservesExistingSshKeepalive guards the non-clobbering half of
// ga-7i1o's fix: the hook runs on every push, so it must not overwrite an
// operator's own core.sshCommand customization just because it already
// mentions ServerAliveInterval under different settings.
func TestPrePushPreservesExistingSshKeepalive(t *testing.T) {
	f := newPrePushFixture(t)
	custom := "ssh -o ServerAliveInterval=5 -o ProxyJump=bastion.example.com"
	f.git(t, "config", "core.sshCommand", custom)

	code, out := f.run(t, "")
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}

	got := f.git(t, "config", "--get", "core.sshCommand")
	if got != custom {
		t.Fatalf("core.sshCommand = %q, want unchanged custom value %q", got, custom)
	}
}
