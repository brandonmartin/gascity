package scripts_test

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/materialize"
)

// materializedSkillName is a stand-in for any entry gc's skill
// materializer creates in a sink. The real names are open-ended (city
// catalog, imported packs, per-agent skills), which is why the ignore
// rules must hide the sink by default rather than enumerate what lands
// in it.
const materializedSkillName = "core.gc-work"

// materializedSkillTarget is the shape of target the materializer points
// its symlinks at: a machine-local cache path that is meaningless in any
// other checkout, and therefore must never reach a commit.
const materializedSkillTarget = "/home/agent/.gc/cache/repos/a82c01c3/skills/gc-work"

// TestGitignoreHidesMaterializedSkillSinks locks this repo's own
// .gitignore against gc's skill materialization.
//
// Every agent session materializes its skill catalog into the provider's
// documented sink under the session WorkDir, and for a worker agent that
// WorkDir is a worktree of this repo. The sink holds one symlink per
// skill plus the .gc-skill-ownership.json bookkeeping file, all pointing
// at machine-local cache paths. None of that is work product, so none of
// it may be visible to git: a sink that git can see makes every
// freshly-created worktree report dirty on a never-edited checkout,
// which defeats dirty-based build provenance, trains every reader to
// ignore `git status` noise, and invites the handoff step's
// "commit whatever is left over" safeguard to commit machine-local cache
// paths onto a branch.
//
// The tracked skill tree under the Claude sink must stay commit-eligible
// at the same time — versioning it is the only reason the sink directory
// is un-ignored at all.
//
// The check runs against a scratch repo seeded with this repo's real
// .gitignore so it exercises git's own traversal semantics (git does not
// descend into an ignored directory) rather than re-implementing them.
func TestGitignoreHidesMaterializedSkillSinks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}

	root := repoRoot(t)
	gitignore, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("read repo .gitignore: %v", err)
	}

	claudeSink, ok := materialize.VendorSink("claude")
	if !ok {
		t.Fatal(`materialize.VendorSink("claude") reports no sink; the tracked skill tree has no home`)
	}
	trackedSkills := trackedPathsUnder(t, root, claudeSink)
	if len(trackedSkills) == 0 {
		t.Fatalf("no tracked files under %s; if this repo no longer versions a skill tree, "+
			"drop the un-ignore rules for that directory from .gitignore instead of leaving "+
			"the materializer's sink exposed", claudeSink)
	}

	repo := t.TempDir()
	skillSinkGit(t, repo, "init", "-q")
	// Ignore the developer's global/system excludes so the fixture measures
	// this repo's .gitignore alone.
	skillSinkGit(t, repo, "config", "core.excludesFile", filepath.Join(repo, ".git", "info", "absent-on-purpose"))
	writeSkillSinkFile(t, filepath.Join(repo, ".gitignore"), string(gitignore))

	// Seed the tracked skill tree: these paths must stay visible.
	for _, rel := range trackedSkills {
		writeSkillSinkFile(t, filepath.Join(repo, filepath.FromSlash(rel)), "tracked skill content\n")
	}

	// Seed every provider sink gc knows how to materialize into. Deriving
	// the list from materialize.SupportedVendors means adding a provider
	// fails this test until .gitignore covers its sink too.
	var mustBeHidden []string
	for _, provider := range materialize.SupportedVendors() {
		sink, ok := materialize.VendorSink(provider)
		if !ok {
			t.Fatalf("materialize.VendorSink(%q) reports no sink but the provider is listed as supported", provider)
		}
		absSink := filepath.Join(repo, filepath.FromSlash(sink))
		if err := os.MkdirAll(absSink, 0o755); err != nil {
			t.Fatalf("create sink %s: %v", sink, err)
		}
		writeSkillSinkFile(t, filepath.Join(absSink, ".gc-skill-ownership.json"),
			`{"targets":{"`+materializedSkillName+`":"`+materializedSkillTarget+`"}}`)
		if err := os.Symlink(materializedSkillTarget, filepath.Join(absSink, materializedSkillName)); err != nil {
			t.Fatalf("create materialized symlink in %s: %v", sink, err)
		}
		mustBeHidden = append(mustBeHidden,
			sink+"/.gc-skill-ownership.json",
			sink+"/"+materializedSkillName,
		)
	}

	untracked := skillSinkUntrackedPaths(t, repo)

	// Fixture sanity: .gitignore itself is untracked in the scratch repo,
	// so an empty status means the fixture broke rather than that the
	// ignore rules work.
	if !untracked[".gitignore"] {
		t.Fatalf("fixture did not produce untracked output; got %v", sortedKeys(untracked))
	}

	for _, rel := range trackedSkills {
		if !untracked[rel] {
			t.Errorf("tracked skill path %s is ignored by .gitignore; it must stay commit-eligible", rel)
		}
	}
	for _, rel := range mustBeHidden {
		if untracked[rel] {
			t.Errorf("gc-materialized path %s is visible to git; every worktree reports dirty on a "+
				"never-edited checkout. Ignore the sink by default and un-ignore only the tracked "+
				"skill tree.", rel)
		}
	}
}

// trackedPathsUnder returns the repo-relative, slash-separated paths git
// tracks under dir, sorted by git's own output order.
func trackedPathsUnder(t *testing.T, root, dir string) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "-z", "--", dir)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files %s: %v", dir, err)
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// skillSinkUntrackedPaths returns the set of paths git reports as
// untracked in repo, with every file listed individually so an untracked
// directory cannot mask what is inside it.
func skillSinkUntrackedPaths(t *testing.T, repo string) map[string]bool {
	t.Helper()
	cmd := exec.Command("git", "status", "--porcelain", "--untracked-files=all")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git status in fixture: %v", err)
	}
	paths := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if rel, found := strings.CutPrefix(line, "?? "); found {
			paths[strings.Trim(rel, `"`)] = true
		}
	}
	return paths
}

func sortedKeys(set map[string]bool) []string {
	return slices.Sorted(maps.Keys(set))
}

func skillSinkGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeSkillSinkFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
