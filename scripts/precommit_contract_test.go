package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreCommitFormatterPreservesFileMode(t *testing.T) {
	repoRoot := repoRoot(t)
	binDir := t.TempDir()
	fakeLint := filepath.Join(binDir, "golangci-lint")
	writeExecutable(t, fakeLint, `#!/usr/bin/env bash
set -euo pipefail
if [ "$#" -ne 2 ] || [ "$1" != "fmt" ] || [ "$2" != "--stdin" ]; then
  echo "unexpected golangci-lint args: $*" >&2
  exit 2
fi
cat
printf '\n'
`)

	source := filepath.Join(t.TempDir(), "needs_format.go")
	if err := os.WriteFile(source, []byte("package main"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	cmd := exec.Command(filepath.Join(repoRoot, "scripts", "precommit-format-staged-go"))
	cmd.Dir = repoRoot
	cmd.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
	}
	cmd.Stdin = strings.NewReader(source + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("precommit formatter failed: %v\n%s", err, out)
	}

	info, err := os.Stat(source)
	if err != nil {
		t.Fatalf("stat formatted source: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("formatted source mode = %o, want 644", got)
	}
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read formatted source: %v", err)
	}
	if string(content) != "package main\n" {
		t.Fatalf("formatted content = %q, want package main with newline", content)
	}
}

func TestTestFastParallelUsesSanitizedEnvironmentAndMachineAwareConcurrency(t *testing.T) {
	repoRoot := repoRoot(t)
	baseEnv := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "LOCAL_TEST_JOBS=") ||
			strings.HasPrefix(entry, "GC_TEST_LOCAL_CPUS=") ||
			strings.HasPrefix(entry, "GC_TEST_LOCAL_MEMORY_KIB=") ||
			strings.HasPrefix(entry, "GC_TEST_LOCAL_MEMINFO=") ||
			strings.HasPrefix(entry, "GC_TEST_LOCAL_PROC_CGROUP=") ||
			strings.HasPrefix(entry, "GC_TEST_LOCAL_CGROUP_ROOT=") ||
			strings.HasPrefix(entry, "GC_PUSH_GATE_NO_CAP=") ||
			strings.HasPrefix(entry, "PUSH_GATE_MAX_CONCURRENT=") ||
			strings.HasPrefix(entry, "PUSH_GATE_MAX_WAIT_SECONDS=") ||
			strings.HasPrefix(entry, "PUSH_GATE_POLL_SECONDS=") ||
			strings.HasPrefix(entry, "PUSH_GATE_UNRELATED_SENTINEL=") {
			continue
		}
		baseEnv = append(baseEnv, entry)
	}
	tests := []struct {
		name      string
		cpus      string
		memoryKiB string
		makeArgs  []string
		wantJobs  string
		cgroup    string
		limit     string
		current   string
	}{
		{name: "large host uses automatic ceiling", cpus: "192", memoryKiB: "536870912", wantJobs: "16"},
		{name: "memory constrains fanout", cpus: "16", memoryKiB: "12582912", wantJobs: "3"},
		{name: "cpu constrains fanout", cpus: "2", memoryKiB: "67108864", wantJobs: "2"},
		{name: "small machine still runs one job", cpus: "8", memoryKiB: "2097152", wantJobs: "1"},
		{name: "unknown memory preserves safe fallback", cpus: "64", memoryKiB: "0", wantJobs: "3"},
		{name: "nested cgroup v2 ancestor constrains fanout", cpus: "16", wantJobs: "3", cgroup: "v2", limit: "12884901888", current: "0"},
		{name: "nested cgroup v1 ancestor constrains fanout", cpus: "16", wantJobs: "2", cgroup: "v1", limit: "8589934592", current: "0"},
		{name: "hybrid cgroup falls through to v1 memory controller", cpus: "16", wantJobs: "3", cgroup: "hybrid", limit: "12884901888", current: "0"},
		{name: "exhausted cgroup forces one job", cpus: "16", wantJobs: "1", cgroup: "v2", limit: "4294967296", current: "4294967296"},
		{name: "explicit override wins", cpus: "192", memoryKiB: "536870912", makeArgs: []string{"LOCAL_TEST_JOBS=7"}, wantJobs: "7"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"-n"}, tt.makeArgs...)
			args = append(args, "test-fast-parallel")
			cmd := exec.Command("make", args...)
			cmd.Dir = repoRoot
			cmd.Env = append(append([]string(nil), baseEnv...),
				"GC_TEST_LOCAL_CPUS="+tt.cpus,
				"GC_PUSH_GATE_NO_CAP=1",
				"PUSH_GATE_MAX_CONCURRENT=7",
				"PUSH_GATE_MAX_WAIT_SECONDS=13",
				"PUSH_GATE_POLL_SECONDS=2",
				"PUSH_GATE_UNRELATED_SENTINEL=must-not-leak",
			)
			if tt.memoryKiB != "" {
				cmd.Env = append(cmd.Env, "GC_TEST_LOCAL_MEMORY_KIB="+tt.memoryKiB)
			}
			if tt.cgroup != "" {
				cmd.Env = append(cmd.Env, localTestCgroupEnv(t, tt.cgroup, tt.limit, tt.current)...)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("make -n test-fast-parallel failed: %v\n%s", err, out)
			}
			command := string(out)
			if !strings.Contains(command, "env -i") {
				t.Fatalf("test-fast-parallel recipe should use TEST_ENV env -i wrapper:\n%s", command)
			}
			if !strings.Contains(command, "./scripts/test-local-parallel fast") {
				t.Fatalf("test-fast-parallel recipe should still dispatch the sharded fast runner:\n%s", command)
			}
			wantJobAssignment := " LOCAL_TEST_JOBS=" + tt.wantJobs + " CMD_GC_PROCESS_TOTAL="
			if !strings.Contains(command, wantJobAssignment) {
				t.Fatalf("test-fast-parallel job count should be %s:\n%s", tt.wantJobs, command)
			}
			for _, key := range []string{
				"GC_PUSH_GATE_NO_CAP",
				"PUSH_GATE_MAX_CONCURRENT",
				"PUSH_GATE_MAX_WAIT_SECONDS",
				"PUSH_GATE_POLL_SECONDS",
			} {
				wantForwarding := key + `="${` + key + `-}"`
				if !strings.Contains(command, wantForwarding) {
					t.Fatalf("test-fast-parallel should forward %s through TEST_ENV:\n%s", key, command)
				}
			}
			if strings.Contains(command, "PUSH_GATE_UNRELATED_SENTINEL") {
				t.Fatalf("test-fast-parallel must keep unrelated ambient variables out of TEST_ENV:\n%s", command)
			}
		})
	}
}

func localTestCgroupEnv(t *testing.T, version, limit, current string) []string {
	t.Helper()
	root := t.TempDir()
	cgroupRoot := filepath.Join(root, "cgroup")
	procCgroup := filepath.Join(root, "proc-self-cgroup")
	meminfo := filepath.Join(root, "meminfo")
	writeTestFile(t, meminfo, "MemAvailable: 67108864 kB\n")

	var controllerRoot, procLine, limitFile, currentFile string
	switch version {
	case "v2":
		controllerRoot = cgroupRoot
		procLine = "0::/parent/child\n"
		limitFile = "memory.max"
		currentFile = "memory.current"
	case "v1":
		controllerRoot = filepath.Join(cgroupRoot, "memory")
		procLine = "5:memory:/parent/child\n"
		limitFile = "memory.limit_in_bytes"
		currentFile = "memory.usage_in_bytes"
	case "hybrid":
		controllerRoot = filepath.Join(cgroupRoot, "memory")
		procLine = "0::/unified/child\n5:memory:/parent/child\n"
		limitFile = "memory.limit_in_bytes"
		currentFile = "memory.usage_in_bytes"
	default:
		t.Fatalf("unsupported cgroup fixture version %q", version)
	}

	writeTestFile(t, procCgroup, procLine)
	if err := os.MkdirAll(filepath.Join(controllerRoot, "parent", "child"), 0o755); err != nil {
		t.Fatalf("create nested cgroup fixture: %v", err)
	}
	writeTestFile(t, filepath.Join(controllerRoot, "parent", limitFile), limit+"\n")
	writeTestFile(t, filepath.Join(controllerRoot, "parent", currentFile), current+"\n")

	return []string{
		"GC_TEST_LOCAL_MEMINFO=" + meminfo,
		"GC_TEST_LOCAL_PROC_CGROUP=" + procCgroup,
		"GC_TEST_LOCAL_CGROUP_ROOT=" + cgroupRoot,
	}
}

func TestPrePushUsesCanonicalMachineAwareConcurrency(t *testing.T) {
	repoRoot := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(repoRoot, ".githooks", "pre-push"))
	if err != nil {
		t.Fatalf("read pre-push hook: %v", err)
	}
	content := string(script)
	if strings.Contains(content, `LOCAL_TEST_JOBS="${LOCAL_TEST_JOBS:-3}"`) {
		t.Fatal("pre-push hook must not replace the canonical machine-aware default with a fixed three-job cap")
	}
	if !strings.Contains(content, "exec make test-fast-parallel") {
		t.Fatal("pre-push hook must continue delegating the unchanged fast-suite inventory to make test-fast-parallel")
	}
	for _, path := range []string{"Makefile", filepath.Join("scripts", "test-local-parallel")} {
		content, err := os.ReadFile(filepath.Join(repoRoot, path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(content), "scripts/test-local-job-count") {
			t.Fatalf("%s must use the canonical machine-aware job detector", path)
		}
	}
}

func TestPreCommitRegeneratesDashboardClientOnSpecChange(t *testing.T) {
	repoRoot := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(repoRoot, ".githooks", "pre-commit"))
	if err != nil {
		t.Fatalf("read pre-commit hook: %v", err)
	}
	content := string(script)

	npmBlockStart := strings.Index(content, "command -v npm")
	if npmBlockStart < 0 {
		t.Fatal("pre-commit hook must guard dashboard regeneration on npm availability")
	}
	npmBlock := content[npmBlockStart:]

	genClientIdx := strings.Index(npmBlock, "npm run generate:client")
	if genClientIdx < 0 {
		t.Fatal("pre-commit hook must run 'npm run generate:client' when internal/api/openapi.json changes — " +
			"make dashboard-check only builds and typechecks against whatever client is already on disk, it never " +
			"regenerates it (that's make dashboard-ci's job, which the hook never calls). A spec-only commit " +
			"currently ships a stale generated TS client (see PR #4627, #4607)")
	}

	dashboardCheckIdx := strings.Index(npmBlock, "make dashboard-check")
	if dashboardCheckIdx < 0 {
		t.Fatal("pre-commit hook must still run make dashboard-check dashboard-smoke")
	}
	if genClientIdx > dashboardCheckIdx {
		t.Fatal("pre-commit hook must regenerate the dashboard client BEFORE typecheck/build, so a client that " +
			"doesn't match the new spec fails typecheck immediately instead of silently building against stale types")
	}

	clientAddNeedle := "git add internal/api/dashboardspa/web/shared/src/generated/gc-supervisor-client"
	genClientAddIdx := strings.Index(npmBlock, clientAddNeedle)
	if genClientAddIdx < 0 {
		t.Fatal("pre-commit hook must stage the regenerated dashboard client so a spec-only commit includes it")
	}
	if genClientAddIdx < genClientIdx {
		t.Fatal("pre-commit hook must stage the generated client after regenerating it, not before")
	}

	if strings.Contains(content, "regenerate the TS types, typecheck, and rebuild") {
		t.Fatal("pre-commit hook's dashboard block comment must not claim it regenerates the TS types unless it " +
			"actually calls npm run generate:client")
	}

	if strings.Contains(content, "warning: npm not on PATH") {
		t.Fatal("pre-commit hook must not downgrade a missing npm to a stderr warning. That warning scrolled " +
			"past unread during `git commit` and let stale generated clients and dist bundles land with no " +
			"local enforcement at all on hosts without Node tooling (ga-fxhk). Fail the commit instead, and " +
			"only for commits that actually stage dashboard-derived sources")
	}

	if !strings.Contains(content, "git commit --no-verify") {
		t.Fatal("pre-commit hook's missing-npm failure must name the deliberate bypass " +
			"(`git commit --no-verify`), so a contributor without Node tooling has a documented way through " +
			"instead of an unexplained wall")
	}
}

func TestPreCommitFailsWhenNpmMissingAndDashboardSourcesStaged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		path  string
		stage string
	}{
		{
			name:  "spec change",
			path:  filepath.Join("internal", "api", "openapi.json"),
			stage: `{"changed":true}` + "\n",
		},
		{
			name:  "spa source change",
			path:  filepath.Join("internal", "api", "dashboardspa", "web", "shared", "src", "added.ts"),
			stage: "export const added = true\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No stubNpm: npm is genuinely absent for this run.
			f := newPreCommitFixture(t)
			writeTestFile(t, filepath.Join(f.repo, tc.path), tc.stage)
			f.git(t, "add", tc.path)

			out, err := f.run(t)
			if err == nil {
				t.Fatalf("pre-commit hook exited 0 with npm absent and %s staged. Without npm neither the "+
					"generated TS client nor internal/api/dashboardspa/dist can be regenerated, so the commit "+
					"lands stale artifacts (#4627, #4607) — dashboard and API-schema changes would get no "+
					"local enforcement whatsoever on this host (ga-fxhk). The hook must fail closed.\n"+
					"hook output:\n%s", tc.path, out)
			}
			if !strings.Contains(string(out), "npm") {
				t.Fatalf("pre-commit hook failed without explaining that npm is the missing prerequisite; "+
					"a bare non-zero exit is not actionable.\nhook output:\n%s", out)
			}
		})
	}
}

func TestPreCommitToleratesMissingNpmWhenNoDashboardSourcesStaged(t *testing.T) {
	// No stubNpm: npm is genuinely absent for this run.
	f := newPreCommitFixture(t)
	writeTestFile(t, filepath.Join(f.repo, "AGENTS.md"), "updated\n")
	f.git(t, "add", "AGENTS.md")

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("pre-commit hook must not block a commit that stages no dashboard-derived sources just "+
			"because npm is absent — requiring Node tooling for Go and docs work is exactly the friction "+
			"the original availability guard existed to avoid: %v\nhook output:\n%s", err, out)
	}
}

func TestPreCommitReachesDashboardBlockWhenOnlySpecFileStaged(t *testing.T) {
	f := newPreCommitFixture(t)
	f.stubNpm(t)

	// Stage ONLY a change to openapi.json -- no .go, web-src, or doc files
	// are staged, matching the reviewer's criterion-2 repro scenario.
	writeTestFile(t, filepath.Join(f.repo, "internal", "api", "openapi.json"), `{"changed":true}`+"\n")
	f.git(t, "add", "internal/api/openapi.json")

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("pre-commit hook failed: %v\n%s", err, out)
	}

	logContent, readErr := f.npmInvocations()
	if readErr != nil {
		t.Fatalf("pre-commit hook exited early and never invoked npm when only internal/api/openapi.json was "+
			"staged -- the go/web/docs early guard must not skip a spec-only commit (hook output: %s)", out)
	}
	if !strings.Contains(logContent, "generate:client") {
		t.Fatalf("pre-commit hook must run 'npm run generate:client' when only internal/api/openapi.json is "+
			"staged, got npm invocations:\n%s", logContent)
	}
}

func TestNativeDoltliteBeadsTargetRunsTaggedSuite(t *testing.T) {
	repoRoot := repoRoot(t)
	makefile, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	if err := validateNativeDoltliteMakefile(string(makefile)); err != nil {
		t.Fatalf("test-native-doltlite-beads recipe: %v", err)
	}

	cmd := exec.Command("make", "-n", "test-native-doltlite-beads")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n test-native-doltlite-beads failed: %v\n%s", err, out)
	}
	command := string(out)
	if err := validateNativeDoltliteDryRun(command); err != nil {
		t.Fatalf("make -n test-native-doltlite-beads output: %v", err)
	}
	for _, want := range []string{
		"CGO_ENABLED=0",
		"-tags gascity_native_beads",
		"-run '^TestDoltlite'",
		"./internal/beads",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("test-native-doltlite-beads recipe missing %q:\n%s", want, command)
		}
	}
	for _, banned := range []string{
		"CGO_ENABLED=1",
		"cgo,gascity_native_beads",
	} {
		if strings.Contains(command, banned) {
			t.Fatalf("test-native-doltlite-beads recipe must not contain %q (doltlite store now uses pure-Go modernc):\n%s", banned, command)
		}
	}
	assertNativeDoltliteBeadsSelectionMatchesTaggedOwners(t, repoRoot)
}

func TestLocalParallelAllowlistIncludesObservableEnv(t *testing.T) {
	repoRoot := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(repoRoot, "scripts", "test-local-parallel"))
	if err != nil {
		t.Fatalf("read test-local-parallel: %v", err)
	}
	content := string(script)
	for _, key := range []string{"OBSERVABLE_TEST_LOG", "OBSERVABLE_FAILURE_LINES"} {
		if !strings.Contains(content, key+"=") {
			t.Fatalf("test-local-parallel job env should pass through %s", key)
		}
	}
	for _, key := range []string{"GC_CITY", "GC_HOME", "GC_SESSION_ID"} {
		if strings.Contains(content, key+"=") {
			t.Fatalf("test-local-parallel job env must not pass through live session env %s", key)
		}
	}
}

// preCommitFixture is a throwaway git repo the repo's real
// .githooks/pre-commit hook can be executed against, plus the stub-binary
// directory that fronts its PATH. npm is absent unless a test calls stubNpm,
// so npm-availability behavior is a property of the test rather than of
// whatever Node tooling the host happens to have installed.
type preCommitFixture struct {
	repo   string
	binDir string
	home   string
	npmLog string
}

// newPreCommitFixture seeds a git repo containing the dashboard-derived paths
// the hook inspects, plus the beads chain the hook resolves relative to
// `git rev-parse --show-toplevel`.
func newPreCommitFixture(t *testing.T) *preCommitFixture {
	t.Helper()
	f := &preCommitFixture{repo: t.TempDir(), binDir: t.TempDir(), home: t.TempDir()}
	f.npmLog = filepath.Join(f.binDir, "npm.log")

	// Install the real forwarder rather than re-implementing it, so a change
	// to the chain's contract surfaces here too.
	chain, err := os.ReadFile(filepath.Join(repoRoot(t), ".githooks", "lib", "beads-chain.sh"))
	if err != nil {
		t.Fatalf("read beads-chain.sh: %v", err)
	}
	chainPath := filepath.Join(f.repo, ".githooks", "lib", "beads-chain.sh")
	if err := os.MkdirAll(filepath.Dir(chainPath), 0o755); err != nil {
		t.Fatalf("mkdir .githooks/lib: %v", err)
	}
	writeExecutable(t, chainPath, string(chain))

	// Stub make: these tests verify the hook's own control flow, not the real
	// check-docs/dashboard-check/dashboard-smoke targets, which need the full repo.
	writeExecutable(t, filepath.Join(f.binDir, "make"), "#!/usr/bin/env bash\nexit 0\n")
	// Stub bd so the chained beads pre-commit hook is a no-op here.
	writeExecutable(t, filepath.Join(f.binDir, "bd"), "#!/usr/bin/env bash\nexit 0\n")

	f.git(t, "init")
	writeTestFile(t, filepath.Join(f.repo, "internal", "api", "openapi.json"), "{}\n")
	writeTestFile(t, filepath.Join(f.repo, "internal", "api", "dashboardspa", "web", "shared", "src", "generated", "gc-supervisor-client"), "placeholder\n")
	writeTestFile(t, filepath.Join(f.repo, "internal", "api", "dashboardspa", "dist", "placeholder"), "placeholder\n")
	writeTestFile(t, filepath.Join(f.repo, "AGENTS.md"), "seed\n")
	f.git(t, "add", "-A")
	f.git(t, "commit", "-m", "init")
	return f
}

// git runs a git command inside the fixture repo and fails the test if it errors.
func (f *preCommitFixture) git(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = f.repo
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.invalid",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// stubNpm makes npm available to the fixture as a stub that records its
// arguments to npmLog and succeeds.
func (f *preCommitFixture) stubNpm(t *testing.T) {
	t.Helper()
	writeExecutable(t, filepath.Join(f.binDir, "npm"), `#!/usr/bin/env bash
set -euo pipefail
echo "$*" >> "`+f.npmLog+`"
exit 0
`)
}

// run executes the repo's real pre-commit hook against the fixture and returns
// its combined output and exit status.
func (f *preCommitFixture) run(t *testing.T) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(repoRoot(t), ".githooks", "pre-commit"))
	cmd.Dir = f.repo
	cmd.Env = []string{
		"PATH=" + f.binDir + string(os.PathListSeparator) + npmFreePath(t),
		"HOME=" + f.home,
	}
	return cmd.CombinedOutput()
}

// npmInvocations returns the recorded stub-npm invocations. The error is
// non-nil when npm was never invoked at all.
func (f *preCommitFixture) npmInvocations() (string, error) {
	content, err := os.ReadFile(f.npmLog)
	return string(content), err
}

// npmFreePath returns PATH with every directory providing an executable npm
// removed, so a fixture that does not call stubNpm runs with npm genuinely
// absent even on a host with Node installed.
func npmFreePath(t *testing.T) string {
	t.Helper()
	var kept []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, "npm"))
		if err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			continue
		}
		kept = append(kept, dir)
	}
	return strings.Join(kept, string(os.PathListSeparator))
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(wd)
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
