package scripts_test

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// cmdGCFastUnitRuntime is the measured wall time of the whole `cmd/gc` package
// under `GC_FAST_UNIT=1`, run alone on an otherwise idle box: 1298.6s (ga-wawj).
// The Makefile's fast unit sweep carries a 15m per-package budget, so cmd/gc
// overran it by ~6.6 minutes with zero failing assertions — deterministically,
// on two independent trees, three separate times. `make test` is the AGENTS.md
// fast unit gate; a gate that can never pass teaches agents to hand-wave it.
const cmdGCFastUnitRuntime = 1299 * time.Second

// cmdGCProcessSerialRuntime is the measured wall time of the same package under
// `GC_FAST_UNIT=0` — the non-short process-backed suite `make test-cmd-gc-process`
// runs — observed at 2081.5s under real fleet load (ga-wawj escalation). That is
// past the 25m budget that target carried, which broke the documented workaround
// for the defect above as well.
const cmdGCProcessSerialRuntime = 2082 * time.Second

// shardBudgetHeadroom is how much slack a per-package `go test -timeout` must
// keep over the honest runtime of the invocation it guards. The flag is a hang
// detector, not a schedule: sized below real runtime it converts every slow but
// healthy run into a false red, and the whole ga-wawj defect is that failure
// mode. Two-times covers the measured spread between an idle box and a loaded
// one (1298s -> 2081s for the same package) without inviting a wedged shard to
// sit for an hour.
const shardBudgetHeadroom = 2

// TestMakeTestNeverSweepsCmdGCAsOneUnshardedPackage guards ga-wawj. `go test`
// applies -timeout per package, so a sweep's budget must cover its slowest
// single package, not its average one. cmd/gc is far slower than every other
// package in the tree and does not fit any budget a fast gate can carry, so it
// must not ride in the monolithic `./...` invocation at all — `test-mac` and
// `test-cover` already drop it for exactly this reason.
func TestMakeTestNeverSweepsCmdGCAsOneUnshardedPackage(t *testing.T) {
	sweep := makeTestSweepCommand(t)

	if strings.Contains(sweep, "./...") {
		t.Fatalf("`make test` still sweeps ./... in one invocation, so cmd/gc rides along with a per-package budget it cannot meet:\n%s", sweep)
	}
	if pkg := cmdGCPackagePattern.FindString(sweep); pkg != "" {
		t.Fatalf("`make test` sweep still lists %s as an ordinary package; it must run sharded instead:\n%s", pkg, sweep)
	}
}

// TestMakeTestStillCoversCmdGCViaShards is the other half of the contract.
// Dropping cmd/gc from the sweep fixes the timeout but would silently gut the
// gate: an agent who edits only cmd/gc would run `make test`, see green, and
// have exercised none of the code they changed. The package has to come back
// as shards, each one a separate invocation with its own per-package budget.
func TestMakeTestStillCoversCmdGCViaShards(t *testing.T) {
	recipe := makeTestDryRunRecipe(t)

	total := cmdGCShardTotal(t, recipe)
	if total < 2 {
		t.Fatalf("`make test` shards cmd/gc %d way(s); sharding into one piece is the unsharded sweep again:\n%s", total, recipe)
	}
	if want := "seq 1 " + strconv.Itoa(total); !strings.Contains(recipe, want) {
		t.Fatalf("`make test` runs cmd/gc shard-of-%d but does not iterate %q, so some shards never run:\n%s", total, want, recipe)
	}
}

// TestMakeTestSharesOneTimeoutAcrossSweepAndShards keeps ga-9au's lesson intact
// through the ga-wawj fix. The sweep and the cmd/gc shards run back to back on
// the same box against the same tree, so a package slow enough to trip one
// should trip the other for the same reason. Two independently maintained
// numbers drift, and that drift is what left the shards on 20m while the unit
// sweep silently kept Go's 10m default.
func TestMakeTestSharesOneTimeoutAcrossSweepAndShards(t *testing.T) {
	recipe := makeTestDryRunRecipe(t)

	sweep := goTestFlagValue(t, makeTestSweepCommand(t), "timeout")
	if sweep == "" {
		t.Fatalf("`make test` sweep passes no -timeout, so it inherits Go's default:\n%s", recipe)
	}
	shard := cmdGCShardTimeout(t, recipe)
	if sweep != shard {
		t.Fatalf("`make test` sweep uses -timeout %q but its cmd/gc shards use GO_TEST_TIMEOUT=%q; the target must carry one budget", sweep, shard)
	}
}

// TestMakeTestShardBudgetCoversMeasuredShardRuntime proves the fix actually
// fits, rather than moving the same overrun behind a loop. A shard runs its
// slice of the package's top-level tests, so its honest cost is the package's
// measured runtime divided by the shard count; the budget has to clear that
// with headroom for a loaded host.
func TestMakeTestShardBudgetCoversMeasuredShardRuntime(t *testing.T) {
	recipe := makeTestDryRunRecipe(t)

	total := cmdGCShardTotal(t, recipe)
	raw := cmdGCShardTimeout(t, recipe)
	budget, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("cmd/gc shard GO_TEST_TIMEOUT=%q is not a Go duration: %v", raw, err)
	}

	want := time.Duration(shardBudgetHeadroom) * (cmdGCFastUnitRuntime / time.Duration(total))
	if budget < want {
		t.Fatalf("cmd/gc shard budget %s does not cover %d-way shards of a %s package with %dx headroom (want >= %s)",
			budget, total, cmdGCFastUnitRuntime, shardBudgetHeadroom, want)
	}
}

// TestCmdGCProcessBudgetCoversMeasuredSerialRuntime guards the second target
// ga-wawj's escalation broke. `make test-cmd-gc-process` is what TESTING.md
// points at as the correct way to cover the process-backed cmd/gc scenarios,
// and it runs the package as one serial invocation, so its budget must clear
// the package's measured serial runtime — not the runtime it had when the
// number was first chosen.
func TestCmdGCProcessBudgetCoversMeasuredSerialRuntime(t *testing.T) {
	recipe := makeDryRunRecipe(t, "test-cmd-gc-process", nil)

	raw := goTestFlagValue(t, recipe, "timeout")
	if raw == "" {
		t.Fatalf("`make test-cmd-gc-process` passes no -timeout to go test:\n%s", recipe)
	}
	budget, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("test-cmd-gc-process -timeout %q is not a Go duration: %v", raw, err)
	}
	if budget <= cmdGCProcessSerialRuntime {
		t.Fatalf("test-cmd-gc-process -timeout = %s, but the package measured %s serially under load; the budget must exceed the runtime it guards",
			budget, cmdGCProcessSerialRuntime)
	}
}

// TestCmdGCProcessBudgetIsOperatorOverridable proves the documented
// `CMD_GC_PROCESS_TIMEOUT=60m make test-cmd-gc-process` escape hatch reaches
// `go test`. The measurement the default is sized from is load-dependent — the
// same package took 1298s idle and 2081s under fleet load — so an operator on a
// slower or busier host needs to raise it without editing the Makefile. A knob
// that is silently dropped is worse than no knob: the operator raises it,
// watches the identical timeout, and has no way to tell it did nothing.
func TestCmdGCProcessBudgetIsOperatorOverridable(t *testing.T) {
	const override = "73m"
	recipe := makeDryRunRecipe(t, "test-cmd-gc-process", []string{"CMD_GC_PROCESS_TIMEOUT=" + override})

	if got := goTestFlagValue(t, recipe, "timeout"); got != override {
		t.Fatalf("test-cmd-gc-process ran with -timeout %q despite CMD_GC_PROCESS_TIMEOUT=%s:\n%s", got, override, recipe)
	}
}

// cmdGCPackagePattern matches cmd/gc named as an ordinary sweep package, in
// either the import-path spelling `go list` emits or the `./cmd/gc` spelling a
// hand-written recipe would use.
var cmdGCPackagePattern = regexp.MustCompile(`(?:github\.com/gastownhall/gascity|\.)/cmd/gc(?:\s|$)`)

// makeTestSweepCommand returns the expanded `go test` sweep line from the
// Makefile's `test` target — the single invocation that carries every package
// the target does not shard.
func makeTestSweepCommand(t *testing.T) string {
	t.Helper()
	recipe := makeTestDryRunRecipe(t)
	for _, line := range strings.Split(recipe, "\n") {
		if strings.Contains(line, "go-test-observable test --") {
			return line
		}
	}
	t.Fatalf("`make -n test` has no `go-test-observable test --` sweep line:\n%s", recipe)
	return ""
}

// cmdGCShardTotal reads the shard count the recipe hands to test-go-test-shard.
func cmdGCShardTotal(t *testing.T, recipe string) int {
	t.Helper()
	match := regexp.MustCompile(`test-go-test-shard\s+\./cmd/gc\s+\S+\s+(\d+)`).FindStringSubmatch(recipe)
	if match == nil {
		t.Fatalf("`make test` never invokes `test-go-test-shard ./cmd/gc`, so the package is not covered at all:\n%s", recipe)
	}
	total, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("cmd/gc shard total %q is not an integer: %v", match[1], err)
	}
	return total
}

// cmdGCShardTimeout reads the per-shard budget the recipe exports to
// test-go-test-shard, which reads it as GO_TEST_TIMEOUT.
func cmdGCShardTimeout(t *testing.T, recipe string) string {
	t.Helper()
	match := regexp.MustCompile(`GO_TEST_TIMEOUT=(\S+)`).FindStringSubmatch(recipe)
	if match == nil {
		t.Fatalf("`make test` cmd/gc shards set no GO_TEST_TIMEOUT, so they inherit the script default independently of the sweep:\n%s", recipe)
	}
	return match[1]
}

// makeTestDryRunRecipe expands the `test` target with `make -n`, so assertions
// read the command lines that actually run rather than the `$(VAR)`
// placeholders in the Makefile source — the package list, the shard count and
// the budget are all variables there.
func makeTestDryRunRecipe(t *testing.T) string {
	t.Helper()
	return makeDryRunRecipe(t, "test", nil)
}

// makeDryRunRecipe expands a target's recipe with `make -n` under the given
// extra environment, for the same reason: every budget in these targets is a
// `?=` variable, so the source text alone does not say what actually runs.
func makeDryRunRecipe(t *testing.T, target string, env []string) string {
	t.Helper()
	cmd := makeCommand("-n", target)
	cmd.Dir = repoRoot(t)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n %s failed: %v\n%s", target, err, out)
	}
	return string(out)
}
