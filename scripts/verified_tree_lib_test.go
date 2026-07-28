package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// verifiedTreeLib is the one definition of "this exact content already passed
// this suite", shared by the parallel runner (which records a verdict) and
// .githooks/pre-push (which consults it).
const verifiedTreeLib = "scripts/lib/verified-tree.sh"

// verifiedTreeRepo is a throwaway git repo. The library's whole contract is
// expressed in terms of real git state — tree identity, cleanliness, and the
// common git dir — so these tests drive a real repo rather than stubbing git.
type verifiedTreeRepo struct {
	dir string
	env []string
}

func newVerifiedTreeRepo(t *testing.T) *verifiedTreeRepo {
	t.Helper()
	dir := t.TempDir()
	confDir := t.TempDir()
	r := &verifiedTreeRepo{
		dir: dir,
		env: append(os.Environ(),
			"GIT_CONFIG_GLOBAL="+filepath.Join(confDir, "gitconfig"),
			"GIT_CONFIG_SYSTEM="+filepath.Join(confDir, "gitconfig-system"),
			// Inherited overrides would silently retune every freshness
			// assertion below.
			"PUSH_GATE_VERIFIED_TTL_SECONDS=",
			"PUSH_GATE_IGNORE_VERIFIED=",
		),
	}
	r.git(t, "init", "-q", "-b", "main")
	r.git(t, "config", "user.email", "test@example.com")
	r.git(t, "config", "user.name", "test")
	r.commit(t, "README.md", "fixture\n")
	return r
}

func (r *verifiedTreeRepo) git(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes a file and commits it, leaving the working tree clean.
func (r *verifiedTreeRepo) commit(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	r.git(t, "add", "-A")
	r.git(t, "commit", "-q", "--no-verify", "-m", "add "+name)
}

// run sources the library in the repo and executes body, returning its
// combined output and exit status.
func (r *verifiedTreeRepo) run(t *testing.T, body string, extraEnv ...string) (string, int) {
	t.Helper()
	lib := filepath.Join(repoRoot(t), verifiedTreeLib)
	cmd := exec.Command("bash", "-c", ". "+lib+"\n"+body)
	cmd.Dir = r.dir
	cmd.Env = append(append([]string{}, r.env...), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exitErr *exec.ExitError
	if !asExitError(err, &exitErr) {
		t.Fatalf("run %s: %v\n%s", verifiedTreeLib, err, out)
	}
	return string(out), exitErr.ExitCode()
}

// fresh reports whether the library considers mode already verified for the
// tree currently checked out.
func (r *verifiedTreeRepo) fresh(t *testing.T, mode string, extraEnv ...string) bool {
	t.Helper()
	out, code := r.run(t, "gc_verified_tree_is_fresh "+mode, extraEnv...)
	switch code {
	case 0:
		return true
	case 1:
		return false
	default:
		t.Fatalf("gc_verified_tree_is_fresh %s exited %d, want 0 or 1\n%s", mode, code, out)
		return false
	}
}

// record runs the record path and fails the test if it reports an error:
// recording is an optimization and must never break the suite that calls it.
func (r *verifiedTreeRepo) record(t *testing.T, args string) {
	t.Helper()
	out, code := r.run(t, "gc_verified_tree_record "+args)
	if code != 0 {
		t.Fatalf("gc_verified_tree_record %s exited %d, want 0\n%s", args, code, out)
	}
}

// markerPath asks the library where mode's marker for the current tree lives.
func (r *verifiedTreeRepo) markerPath(t *testing.T, mode string) string {
	t.Helper()
	out, code := r.run(t, `gc_verified_tree_marker `+mode+` "$(gc_verified_tree_id)"`)
	if code != 0 {
		t.Fatalf("gc_verified_tree_marker exited %d\n%s", code, out)
	}
	return strings.TrimSpace(out)
}

// TestVerifiedTreeRecordMakesTheSameTreeFresh is the dedup this library exists
// for (ga-4rr4): the refinery's merge gate and the pre-push gate run the
// identical fast suite over the identical tree, so the second run must be able
// to see that the first one already returned green.
func TestVerifiedTreeRecordMakesTheSameTreeFresh(t *testing.T) {
	r := newVerifiedTreeRepo(t)

	if r.fresh(t, "fast") {
		t.Fatalf("tree reported fresh before any suite recorded a verdict")
	}
	r.record(t, "fast")
	if !r.fresh(t, "fast") {
		t.Fatalf("tree not fresh immediately after recording a green fast suite")
	}
}

// TestVerifiedTreeIsScopedToTheSuiteThatRan keeps a cheap suite from vouching
// for an expensive one. `fast` and `full` judge different amounts of the tree;
// a `fast` verdict must not satisfy a gate that wanted `full`.
func TestVerifiedTreeIsScopedToTheSuiteThatRan(t *testing.T) {
	r := newVerifiedTreeRepo(t)
	r.record(t, "fast")

	if r.fresh(t, "full") {
		t.Fatalf("a green `fast` run satisfied a `full` gate")
	}
	if !r.fresh(t, "fast") {
		t.Fatalf("`fast` gate not satisfied by its own recorded verdict")
	}
}

// TestVerifiedTreeIsNotFreshAfterTheTreeChanges is the safety property that
// makes this dedup sound at all: the marker vouches for one exact content
// hash, so any new commit invalidates it without needing to be invalidated.
func TestVerifiedTreeIsNotFreshAfterTheTreeChanges(t *testing.T) {
	r := newVerifiedTreeRepo(t)
	r.record(t, "fast")

	r.commit(t, "added.go", "package fixture\n")

	if r.fresh(t, "fast") {
		t.Fatalf("stale marker vouched for a tree that gained a new commit")
	}
}

// TestVerifiedTreeRefusesToNameADirtyTree covers the case where HEAD's tree
// object is not what the suite would actually run against. Uncommitted edits
// are invisible to a commit-tree hash, so keying a verdict on one would let
// untested content ride out on a marker earned by different content.
func TestVerifiedTreeRefusesToNameADirtyTree(t *testing.T) {
	r := newVerifiedTreeRepo(t)
	r.record(t, "fast")
	if !r.fresh(t, "fast") {
		t.Fatalf("precondition: clean tree should be fresh after recording")
	}

	if err := os.WriteFile(filepath.Join(r.dir, "dirty.go"), []byte("package fixture\n"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	out, code := r.run(t, "gc_verified_tree_id")
	if code == 0 {
		t.Fatalf("gc_verified_tree_id named a dirty tree (%q); uncommitted work is not covered by HEAD's tree hash", strings.TrimSpace(out))
	}
	if r.fresh(t, "fast") {
		t.Fatalf("dirty tree reported fresh; untested edits would skip the gate")
	}
}

// TestVerifiedTreeRecordSkipsWhenTheSuiteChangedTheTree guards the record side
// of the same property. The runner captures the tree it is about to judge and
// hands it back at the end; if the content moved underneath the suite, the
// verdict belongs to neither tree and must not be written.
func TestVerifiedTreeRecordSkipsWhenTheSuiteChangedTheTree(t *testing.T) {
	r := newVerifiedTreeRepo(t)
	stale := strings.Repeat("a", 40)

	r.record(t, "fast "+stale)

	if r.fresh(t, "fast") {
		t.Fatalf("recorded a verdict for a tree that differs from the one the suite started on")
	}
}

// TestVerifiedTreeMarkerExpires bounds how long a verdict speaks for. The
// content is unchanged, but the toolchain and host around it are not pinned by
// a tree hash, so an indefinitely valid marker would eventually vouch for a
// build nobody has actually run.
func TestVerifiedTreeMarkerExpires(t *testing.T) {
	r := newVerifiedTreeRepo(t)
	r.record(t, "fast")
	marker := r.markerPath(t, "fast")

	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatalf("backdate marker %s: %v", marker, err)
	}

	if r.fresh(t, "fast") {
		t.Fatalf("a 48h-old marker still counted as fresh under the default TTL")
	}
	if !r.fresh(t, "fast", "PUSH_GATE_VERIFIED_TTL_SECONDS=604800") {
		t.Fatalf("a 48h-old marker was rejected under an explicit 7d TTL")
	}
}

// TestVerifiedTreeZeroTTLDisablesReuse gives operators an off switch that does
// not require deleting state: a zero TTL means every marker is already expired.
func TestVerifiedTreeZeroTTLDisablesReuse(t *testing.T) {
	r := newVerifiedTreeRepo(t)
	r.record(t, "fast")

	if r.fresh(t, "fast", "PUSH_GATE_VERIFIED_TTL_SECONDS=0") {
		t.Fatalf("marker honored under PUSH_GATE_VERIFIED_TTL_SECONDS=0")
	}
}

// TestVerifiedTreeIgnoreOverrideForcesRerun is the per-invocation escape
// hatch. Someone debugging a suspected flake needs a way to make the gate run
// for real without hunting for marker files.
func TestVerifiedTreeIgnoreOverrideForcesRerun(t *testing.T) {
	r := newVerifiedTreeRepo(t)
	r.record(t, "fast")

	if r.fresh(t, "fast", "PUSH_GATE_IGNORE_VERIFIED=1") {
		t.Fatalf("marker honored despite PUSH_GATE_IGNORE_VERIFIED=1")
	}
}

// TestVerifiedTreeMarkersLiveOutsideTheWorktree keeps this cache out of the
// content it vouches for. A marker written into the worktree would show up as
// an untracked file — which is itself a reason to refuse the tree — and could
// be committed by an `add -A`.
func TestVerifiedTreeMarkersLiveOutsideTheWorktree(t *testing.T) {
	r := newVerifiedTreeRepo(t)
	r.record(t, "fast")

	marker := r.markerPath(t, "fast")
	gitDir := r.git(t, "rev-parse", "--absolute-git-dir")
	if !strings.HasPrefix(marker, gitDir+string(os.PathSeparator)) {
		t.Fatalf("marker %q is not under the git dir %q", marker, gitDir)
	}
	if got := r.git(t, "status", "--porcelain"); got != "" {
		t.Fatalf("recording a verdict dirtied the worktree: %q", got)
	}
}

// TestVerifiedTreeRecordNeverFailsItsCaller pins the failure posture. The
// runner sources this library under `set -e`, so a record path that can return
// nonzero would turn a green suite into a failed one over a cache write.
func TestVerifiedTreeRecordNeverFailsItsCaller(t *testing.T) {
	outside := t.TempDir()
	lib := filepath.Join(repoRoot(t), verifiedTreeLib)
	cmd := exec.Command("bash", "-c", "set -euo pipefail\n. "+lib+"\ngc_verified_tree_record fast\necho reached-the-end")
	cmd.Dir = outside
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(outside, "gitconfig"),
		// git ceiling stops the temp dir from resolving to some enclosing repo.
		"GIT_CEILING_DIRECTORIES="+filepath.Dir(outside),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("record outside a git repo aborted its caller: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "reached-the-end") {
		t.Fatalf("record outside a git repo did not return control to its caller:\n%s", out)
	}
}

// TestVerifiedTreePrunesExpiredMarkers keeps the cache from growing without
// bound: every distinct tree ever verified would otherwise leave a file behind
// forever.
func TestVerifiedTreePrunesExpiredMarkers(t *testing.T) {
	r := newVerifiedTreeRepo(t)
	r.record(t, "fast")
	stale := filepath.Join(filepath.Dir(r.markerPath(t, "fast")), "fast."+strings.Repeat("b", 40))
	if err := os.WriteFile(stale, []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("backdate stale marker: %v", err)
	}

	r.commit(t, "added.go", "package fixture\n")
	r.record(t, "fast")

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("expired marker %s survived a later record (stat err = %v)", stale, err)
	}
	if !r.fresh(t, "fast") {
		t.Fatalf("pruning removed the marker that was just recorded")
	}
}

// TestLocalParallelRecordsVerifiedTreeOnGreen ties the runner to this library.
// The runner is the only place that knows a suite passed, so if it stops
// recording, the pre-push gate silently goes back to running every suite twice
// — with no test failing to say so.
func TestLocalParallelRecordsVerifiedTreeOnGreen(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "test-local-parallel"))
	if err != nil {
		t.Fatalf("read scripts/test-local-parallel: %v", err)
	}
	runner := string(body)

	if !strings.Contains(runner, verifiedTreeLib) {
		t.Fatalf("scripts/test-local-parallel does not source %s, so no suite ever records a verdict", verifiedTreeLib)
	}
	if !strings.Contains(runner, "gc_verified_tree_record") {
		t.Fatalf("scripts/test-local-parallel never calls gc_verified_tree_record")
	}

	// The verdict must be recorded only where the runner knows the suite was
	// green. Anchor on the success branch's own marker line.
	green := strings.Index(runner, `echo "All ${mode} jobs passed"`)
	if green < 0 {
		t.Fatalf("scripts/test-local-parallel no longer has the success epilogue this test anchors on")
	}
	record := strings.Index(runner, "gc_verified_tree_record")
	if record < 0 || record > green {
		t.Fatalf("gc_verified_tree_record is not on the runner's green path (record at %d, green epilogue at %d)", record, green)
	}
	failure := strings.Index(runner, "gc_report_suite_outcome")
	if failure >= 0 && record > failure {
		t.Fatalf("gc_verified_tree_record appears after the failure epilogue; a red suite must not record a verdict")
	}
}
