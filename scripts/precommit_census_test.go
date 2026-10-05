package scripts_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	censusPackage = "./internal/testpolicy/resourcecensus"
	censusCommand = "test " + censusPackage + " -run ^TestRepositoryLedgerMatchesCensusAndDocumentation$ -count=1"
	censusDrift   = "source resource census grew: scope=untagged resource=fixed_sleep calls=320 (baseline 319), files=122 (baseline 122)"
)

// censusHookRepo is a throwaway repository carrying the real pre-commit hook.
// Every tool the hook can reach (go, make, bd) is a stub, so the tests observe
// only the hook's own control flow: which staged paths make it run the
// resource census, and what it does when the census fails.
type censusHookRepo struct {
	hook   string
	repo   string
	goLog  string
	binDir string
}

func newCensusHookRepo(t *testing.T, censusExit int) *censusHookRepo {
	t.Helper()
	root := repoRoot(t)
	repo := t.TempDir()
	goLog := filepath.Join(t.TempDir(), "go.log")

	censusBranch := ":"
	if censusExit != 0 {
		censusBranch = fmt.Sprintf("printf '%%s\\n' %q; exit %d", censusDrift, censusExit)
	}
	goStub := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
printf '%%s\n' "$*" >> %q
if [ "${1:-}" = "test" ] && [ "${2:-}" = %q ]; then
  %s
fi
exit 0
`, goLog, censusPackage, censusBranch)

	r := &censusHookRepo{
		hook:  filepath.Join(root, ".githooks", "pre-commit"),
		repo:  repo,
		goLog: goLog,
		binDir: restrictedPathWithoutNpm(t, map[string]string{
			"go":   goStub,
			"make": "#!/usr/bin/env bash\nexit 0\n",
		}),
	}

	installBeadsChainForTempRepo(t, root, repo)
	r.git(t, "init")
	if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o755); err != nil {
		t.Fatalf("create scripts dir: %v", err)
	}
	writeExecutable(t, filepath.Join(repo, "scripts", "precommit-format-staged-go"), "#!/usr/bin/env bash\nexit 0\n")
	// The hook's Go block unconditionally `git add`s every generated artifact,
	// so each one must exist for it to reach the steps this test cares about.
	for _, name := range []string{
		"internal/api/openapi.json",
		"docs/reference/schema/openapi.json",
		"docs/reference/schema/openapi.txt",
		"internal/api/genclient/client_gen.go",
		"docs/reference/schema/city-schema.json",
		"docs/reference/schema/city-schema.txt",
		"docs/reference/config.md",
		"docs/reference/cli.md",
		"pkg/example.go",
		"pkg/example_test.go",
		"test/test-resources.toml",
		"TESTING.md",
		"README.md",
		"notes.txt",
		"internal/testpolicy/resourcecensus/BUILD.bazel",
	} {
		writeTestFile(t, filepath.Join(repo, name), "initial\n")
	}
	r.git(t, "add", "-A")
	r.git(t, "commit", "-m", "init")
	return r
}

func (r *censusHookRepo) git(t *testing.T, args ...string) {
	t.Helper()
	cmd := testCommand("git", args...)
	cmd.Dir = r.repo
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.invalid",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// edit rewrites each path and stages it.
func (r *censusHookRepo) edit(t *testing.T, paths ...string) {
	t.Helper()
	for _, name := range paths {
		writeTestFile(t, filepath.Join(r.repo, name), "changed\n")
		r.git(t, "add", name)
	}
}

// runHook executes the pre-commit hook against the staged set and returns its
// combined output, every `go` invocation it made, and its exit error.
func (r *censusHookRepo) runHook(t *testing.T) (string, []string, error) {
	t.Helper()
	cmd := testCommand("bash", r.hook)
	cmd.Dir = r.repo
	cmd.Env = []string{"PATH=" + r.binDir, "HOME=" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	logged, readErr := os.ReadFile(r.goLog)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read go stub log: %v", readErr)
	}
	return string(out), strings.Split(strings.TrimSpace(string(logged)), "\n"), err
}

func countCalls(calls []string, want string) int {
	n := 0
	for _, call := range calls {
		if call == want {
			n++
		}
	}
	return n
}

func TestPreCommitRunsResourceCensusOnlyWhenCensusInputsAreStaged(t *testing.T) {
	tests := []struct {
		name       string
		stage      func(t *testing.T, r *censusHookRepo)
		wantCensus bool
	}{
		{
			name:       "go test file",
			stage:      func(t *testing.T, r *censusHookRepo) { r.edit(t, "pkg/example_test.go") },
			wantCensus: true,
		},
		{
			name:       "non-test go file",
			stage:      func(t *testing.T, r *censusHookRepo) { r.edit(t, "pkg/example.go") },
			wantCensus: true,
		},
		{
			name:       "ledger manifest",
			stage:      func(t *testing.T, r *censusHookRepo) { r.edit(t, "test/test-resources.toml") },
			wantCensus: true,
		},
		{
			name:       "TESTING.md ledger block",
			stage:      func(t *testing.T, r *censusHookRepo) { r.edit(t, "TESTING.md") },
			wantCensus: true,
		},
		{
			name: "census package file",
			stage: func(t *testing.T, r *censusHookRepo) {
				r.edit(t, "internal/testpolicy/resourcecensus/BUILD.bazel")
			},
			wantCensus: true,
		},
		{
			name:       "deleted go test file",
			stage:      func(t *testing.T, r *censusHookRepo) { r.git(t, "rm", "pkg/example_test.go") },
			wantCensus: true,
		},
		{
			name: "go test file renamed to a non-go name",
			stage: func(t *testing.T, r *censusHookRepo) {
				r.git(t, "mv", "pkg/example_test.go", "pkg/example_test.txt")
			},
			wantCensus: true,
		},
		{
			name:       "go file and ledger together run the census once",
			stage:      func(t *testing.T, r *censusHookRepo) { r.edit(t, "pkg/example_test.go", "test/test-resources.toml") },
			wantCensus: true,
		},
		{
			name:       "docs only",
			stage:      func(t *testing.T, r *censusHookRepo) { r.edit(t, "README.md") },
			wantCensus: false,
		},
		{
			name:       "unrelated file",
			stage:      func(t *testing.T, r *censusHookRepo) { r.edit(t, "notes.txt") },
			wantCensus: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newCensusHookRepo(t, 0)
			tt.stage(t, r)

			out, calls, err := r.runHook(t)
			if err != nil {
				t.Fatalf("pre-commit hook failed: %v\n%s", err, out)
			}
			want := 0
			if tt.wantCensus {
				want = 1
			}
			if got := countCalls(calls, censusCommand); got != want {
				t.Fatalf("resource census ran %d times, want %d; go invocations:\n%s", got, want, strings.Join(calls, "\n"))
			}
		})
	}
}

func TestPreCommitBlocksCommitAndExplainsRemediationWhenCensusFails(t *testing.T) {
	r := newCensusHookRepo(t, 1)
	r.edit(t, "pkg/example_test.go")

	out, calls, err := r.runHook(t)
	if err == nil {
		t.Fatalf("pre-commit hook must block the commit when the resource census fails, output:\n%s", out)
	}
	for _, want := range []string{
		censusDrift,
		"census.go",
		"test/test-resources.toml",
		"-update",
		"Never raise a baseline",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("census failure output must contain %q, got:\n%s", want, out)
		}
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "run ./cmd/") || strings.HasPrefix(call, "generate ") {
			t.Fatalf("census gate must fail before the codegen steps, but the hook ran `go %s`", call)
		}
	}
}

func TestPreCommitSkipsCensusHintWhenCensusPasses(t *testing.T) {
	r := newCensusHookRepo(t, 0)
	r.edit(t, "pkg/example_test.go")

	out, _, err := r.runHook(t)
	if err != nil {
		t.Fatalf("pre-commit hook failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "Never raise a baseline") {
		t.Fatalf("remediation hint must only print when the census fails, got:\n%s", out)
	}
}
