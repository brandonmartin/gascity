package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/publishgate"
)

const (
	publishGateTestHead      = "f5289f71058c6cf5236b0c50df0563c6b0bc9191"
	publishGateTestPreRebase = "ec90bac29981c4f6a1da2fa6e6d7228a0be6d31a"
	publishGateTestTarget    = "1bc642727251a84a33d0316a0d3d26e3e9c11ffe"
)

// publishGateFakeRepo answers ref lookups from fixed maps so the stamp can
// be tested without a worktree.
type publishGateFakeRepo struct {
	branch string
	refs   map[string]string
	counts map[string]int
}

func (f *publishGateFakeRepo) CurrentBranch() (string, error) {
	if f.branch == "" {
		return "", fmt.Errorf("detached HEAD")
	}
	return f.branch, nil
}

func (f *publishGateFakeRepo) ResolveRef(ref string) (string, error) {
	if sha, ok := f.refs[ref]; ok {
		return sha, nil
	}
	return "", fmt.Errorf("unresolved ref %s", ref)
}

func (f *publishGateFakeRepo) CountBehind(from, to string) (int, error) {
	if n, ok := f.counts[from+".."+to]; ok {
		return n, nil
	}
	return 0, fmt.Errorf("no count for %s..%s", from, to)
}

// haltWorktree is the shape a halting worker presents: standing on the
// artifact branch, which has never been pushed.
func haltWorktree() *publishGateFakeRepo {
	return &publishGateFakeRepo{
		branch: "polecat/ga-qbq",
		refs: map[string]string{
			"HEAD":                        publishGateTestHead,
			"refs/remotes/origin/develop": publishGateTestTarget,
			"refs/remotes/upstream/main":  publishGateTestTarget,
		},
	}
}

func withPublishGateStamp(t *testing.T, repo publishgate.HeadResolver, now time.Time) {
	t.Helper()
	prevResolver := newPublishGateStampResolver
	prevNow := publishGateStampNow
	newPublishGateStampResolver = func(string) publishgate.HeadResolver { return repo }
	publishGateStampNow = func() time.Time { return now }
	t.Cleanup(func() {
		newPublishGateStampResolver = prevResolver
		publishGateStampNow = prevNow
	})
}

// stampedMetadata collects the key=value pairs a rewritten argv sets.
func stampedMetadata(args []string) map[string]string {
	out := map[string]string{}
	for i := 0; i < len(args); i++ {
		var pair string
		switch {
		case args[i] == "--set-metadata" && i+1 < len(args):
			i++
			pair = args[i]
		case strings.HasPrefix(args[i], "--set-metadata="):
			pair = strings.TrimPrefix(args[i], "--set-metadata=")
		default:
			continue
		}
		if k, v, ok := strings.Cut(pair, "="); ok {
			out[k] = v
		}
	}
	return out
}

func TestStampPublishGateArgsRecordsProvenance(t *testing.T) {
	now := time.Date(2026, 7, 26, 6, 15, 0, 0, time.UTC)
	withPublishGateStamp(t, haltWorktree(), now)

	got := stampPublishGateArgs([]string{
		"update", "ga-qbq",
		"--status=open",
		"--set-metadata", "branch=polecat/ga-qbq",
		"--set-metadata", "target=develop",
		"--set-metadata", "branch_ready=true",
		"--set-metadata", "halt_reason=auto_push_false",
	}, "/work/ga-qbq")

	meta := stampedMetadata(got)
	if meta[publishgate.MetaBranchReadyAt] != "2026-07-26T06:15:00Z" {
		t.Errorf("%s = %q, want the pinned instant", publishgate.MetaBranchReadyAt, meta[publishgate.MetaBranchReadyAt])
	}
	if meta[publishgate.MetaCommit] != publishGateTestHead {
		t.Errorf("%s = %q, want HEAD %s", publishgate.MetaCommit, meta[publishgate.MetaCommit], publishGateTestHead)
	}
	if meta[publishgate.MetaTargetHead] != publishGateTestTarget {
		t.Errorf("%s = %q, want the target tip %s", publishgate.MetaTargetHead, meta[publishgate.MetaTargetHead], publishGateTestTarget)
	}
	// The branch was never pushed, so following metadata.branch would not
	// reach the artifact.
	if meta[publishgate.MetaBranchStale] != "true" {
		t.Errorf("%s = %q, want true for an unpushed branch", publishgate.MetaBranchStale, meta[publishgate.MetaBranchStale])
	}
	// The caller's own edits survive untouched and in order.
	if !strings.Contains(strings.Join(got, " "), "--status=open --set-metadata branch=polecat/ga-qbq") {
		t.Errorf("original args were not preserved: %v", got)
	}
}

func TestStampPublishGateArgsMarksAPublishedBranchAuthoritative(t *testing.T) {
	repo := haltWorktree()
	repo.refs["refs/remotes/origin/polecat/ga-qbq"] = publishGateTestHead
	withPublishGateStamp(t, repo, time.Now())

	meta := stampedMetadata(stampPublishGateArgs([]string{
		"update", "ga-qbq",
		"--set-metadata", "branch=polecat/ga-qbq",
		"--set-metadata", "target=develop",
		"--set-metadata", "branch_ready=true",
	}, "/work/ga-qbq"))

	if meta[publishgate.MetaBranchStale] != "false" {
		t.Fatalf("%s = %q, want false when the remote branch is the artifact", publishgate.MetaBranchStale, meta[publishgate.MetaBranchStale])
	}
}

// The ga-b5h shape: the branch was pushed at some point but has since been
// rebased, so origin holds pre-rebase history.
func TestStampPublishGateArgsMarksPreRebaseBranchStale(t *testing.T) {
	repo := haltWorktree()
	repo.refs["refs/remotes/origin/polecat/ga-qbq"] = publishGateTestPreRebase
	withPublishGateStamp(t, repo, time.Now())

	meta := stampedMetadata(stampPublishGateArgs([]string{
		"update", "ga-qbq",
		"--set-metadata", "branch=polecat/ga-qbq",
		"--set-metadata", "branch_ready=true",
	}, "/work/ga-qbq"))

	if meta[publishgate.MetaBranchStale] != "true" {
		t.Fatalf("%s = %q, want true when origin holds pre-rebase history", publishgate.MetaBranchStale, meta[publishgate.MetaBranchStale])
	}
	if meta[publishgate.MetaCommit] != publishGateTestHead {
		t.Fatalf("%s = %q, want the worktree HEAD", publishgate.MetaCommit, meta[publishgate.MetaCommit])
	}
}

// Only a caller standing in the artifact's own worktree may contribute
// git-derived provenance; anyone else gets the timestamp and nothing more.
func TestStampPublishGateArgsRequiresTheArtifactWorktree(t *testing.T) {
	repo := haltWorktree()
	repo.branch = "some-other-branch"
	withPublishGateStamp(t, repo, time.Now())

	meta := stampedMetadata(stampPublishGateArgs([]string{
		"update", "ga-qbq",
		"--set-metadata", "branch=polecat/ga-qbq",
		"--set-metadata", "target=develop",
		"--set-metadata", "branch_ready=true",
	}, "/somewhere/else"))

	if _, ok := meta[publishgate.MetaBranchReadyAt]; !ok {
		t.Error("branch_ready_at should still be stamped without a repository")
	}
	for _, key := range []string{publishgate.MetaCommit, publishgate.MetaTargetHead, publishgate.MetaBranchStale} {
		if _, ok := meta[key]; ok {
			t.Errorf("%s = %q, want it omitted outside the artifact worktree", key, meta[key])
		}
	}
}

func TestStampPublishGateArgsNeverOverridesCallerValues(t *testing.T) {
	withPublishGateStamp(t, haltWorktree(), time.Date(2026, 7, 26, 6, 15, 0, 0, time.UTC))

	args := []string{
		"update", "ga-qbq",
		"--set-metadata", "branch=polecat/ga-qbq",
		"--set-metadata", "target=develop",
		"--set-metadata", "branch_ready=true",
		"--set-metadata", "branch_ready_at=2026-07-11T09:30:00Z",
		"--set-metadata", "commit=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		"--set-metadata", "target_head=cafebabecafebabecafebabecafebabecafebabe",
		"--set-metadata", "branch_stale=false",
	}
	got := stampPublishGateArgs(args, "/work/ga-qbq")

	if len(got) != len(args) {
		t.Fatalf("args grew from %d to %d; nothing should be added when the caller set every key:\n%v", len(args), len(got), got)
	}
	meta := stampedMetadata(got)
	if meta[publishgate.MetaBranchReadyAt] != "2026-07-11T09:30:00Z" {
		t.Errorf("branch_ready_at = %q, want the caller's value", meta[publishgate.MetaBranchReadyAt])
	}
}

// Running an already-stamped command through again must be a no-op; the
// halt sequence is re-run after crashes.
func TestStampPublishGateArgsIsIdempotent(t *testing.T) {
	withPublishGateStamp(t, haltWorktree(), time.Date(2026, 7, 26, 6, 15, 0, 0, time.UTC))

	once := stampPublishGateArgs([]string{
		"update", "ga-qbq",
		"--set-metadata", "branch=polecat/ga-qbq",
		"--set-metadata", "target=develop",
		"--set-metadata", "branch_ready=true",
	}, "/work/ga-qbq")
	twice := stampPublishGateArgs(once, "/work/ga-qbq")

	if strings.Join(once, " ") != strings.Join(twice, " ") {
		t.Fatalf("second stamp changed the args:\n once: %v\ntwice: %v", once, twice)
	}
}

func TestStampPublishGateArgsPassesThroughUnrelatedCommands(t *testing.T) {
	withPublishGateStamp(t, haltWorktree(), time.Now())

	cases := [][]string{
		nil,
		{"list", "--status", "open"},
		{"close", "ga-qbq"},
		{"update", "ga-qbq", "--status=open"},
		{"update", "ga-qbq", "--set-metadata", "branch_ready=false"},
		{"update", "ga-qbq", "--set-metadata", "target=develop"},
		// bd rejects --metadata combined with --set-metadata, so a JSON-form
		// write must not be augmented.
		{"update", "ga-qbq", "--metadata", `{"branch_ready":"true"}`},
	}
	for _, args := range cases {
		got := stampPublishGateArgs(args, "/work/ga-qbq")
		if strings.Join(got, " ") != strings.Join(args, " ") {
			t.Errorf("stampPublishGateArgs(%v) = %v, want it unchanged", args, got)
		}
	}
}

func TestStampPublishGateArgsAcceptsEqualsForm(t *testing.T) {
	withPublishGateStamp(t, haltWorktree(), time.Date(2026, 7, 26, 6, 15, 0, 0, time.UTC))

	meta := stampedMetadata(stampPublishGateArgs([]string{
		"update", "ga-qbq",
		"--set-metadata=branch=polecat/ga-qbq",
		"--set-metadata=branch_ready=true",
	}, "/work/ga-qbq"))

	if meta[publishgate.MetaCommit] != publishGateTestHead {
		t.Fatalf("commit = %q, want the equals-form write to be recognized", meta[publishgate.MetaCommit])
	}
}

// Without a recorded branch there is no proof of which worktree the caller
// is in, so no git-derived key may be trusted.
func TestStampPublishGateArgsWithoutBranchStampsTimeOnly(t *testing.T) {
	withPublishGateStamp(t, haltWorktree(), time.Date(2026, 7, 26, 6, 15, 0, 0, time.UTC))

	meta := stampedMetadata(stampPublishGateArgs([]string{
		"update", "ga-qbq", "--set-metadata", "branch_ready=true",
	}, "/work/ga-qbq"))

	if len(meta) != 2 || meta[publishgate.MetaBranchReadyAt] == "" {
		t.Fatalf("metadata = %v, want only branch_ready plus a timestamp", meta)
	}
}
