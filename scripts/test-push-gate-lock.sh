#!/usr/bin/env bash
#
# test-push-gate-lock.sh — unit tests for the pre-push gate's cross-invocation
# concurrency bound (ga-owh20p) plus static assertions that it's wired into
# scripts/test-local-parallel correctly.
#
# The slot mechanics live in scripts/push-gate-lock-lib.sh and are exercised
# directly here — no real city, no real test suite run. Cross-process denial
# is tested deterministically with flock(1) probes, so there is no reliance
# on timing EXCEPT the one case that inherently requires wall-clock: the
# wait-then-timeout path, which uses second-scale overrides
# (PUSH_GATE_MAX_WAIT_SECONDS / PUSH_GATE_POLL_SECONDS) to stay fast.
#
# Coverage: acquire/hold/deny/release, the bounded wait and its return code,
# FD inheritance (a detached descendant must not pin a slot), dead-holder
# diagnostics, the missing-flock degrade path, malformed tunables, the
# GC_PUSH_GATE_NO_CAP escape hatch, both city-root resolution modes, the
# slots-dir fallback, and static assertions that scripts/test-local-parallel
# wires all of it up — including closing the gate FD before the fan-out.

set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="$TEST_DIR/push-gate-lock-lib.sh"
LOCAL_PARALLEL="$TEST_DIR/test-local-parallel"

# shellcheck source=./push-gate-lock-lib.sh disable=SC1091
. "$LIB"

pass=0; fail=0
record_pass() { echo "  ok   $1"; pass=$((pass + 1)); }
record_fail() { echo "  FAIL $1 — $2"; fail=$((fail + 1)); }

assert_eq() {
    local name="$1" got="$2" want="$3"
    if [[ "$got" == "$want" ]]; then record_pass "$name"
    else record_fail "$name" "got '$got', want '$want'"; fi
}
assert_true()  { if "${@:2}"; then record_pass "$1"; else record_fail "$1" "expected true"; fi; }
assert_false() { if "${@:2}"; then record_fail "$1" "expected false"; else record_pass "$1"; fi; }
assert_contains() {
    local name="$1" haystack="$2" needle="$3"
    if [[ "$haystack" == *"$needle"* ]]; then record_pass "$name"
    else record_fail "$name" "missing '$needle' in: $haystack"; fi
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
SLOTS="$WORK/gate-slots"

# ---------------- acquire / hold (two slots) ----------------
export PUSH_GATE_MAX_CONCURRENT=2
FD0=""
if push_gate_acquire_slot "$SLOTS" FD0 "holder-A"; then
    record_pass "acquire.first_succeeds"
else
    record_fail "acquire.first_succeeds" "expected return 0"
fi
assert_true "acquire.fd_var_set" test -n "$FD0"

FD1=""
if push_gate_acquire_slot "$SLOTS" FD1 "holder-B"; then
    record_pass "acquire.second_succeeds_distinct_slot"
else
    record_fail "acquire.second_succeeds_distinct_slot" "expected return 0 (max=2)"
fi
assert_true "acquire.second_fd_differs" test "$FD0" != "$FD1"

HOLDERS="$(push_gate_describe_slots "$SLOTS" 2)"
assert_contains "describe.has_pid_a"   "$HOLDERS" "$$"
assert_contains "describe.has_label_a" "$HOLDERS" "holder-A"
assert_contains "describe.has_label_b" "$HOLDERS" "holder-B"

# ---------------- denial while both slots held (cross-process, deterministic) ----------------
if flock -n "$SLOTS/slot-0.lock" -c 'exit 0'; then
    record_fail "deny.flock_probe_slot0" "child acquired a slot we hold"
else
    record_pass "deny.flock_probe_slot0"
fi
if flock -n "$SLOTS/slot-1.lock" -c 'exit 0'; then
    record_fail "deny.flock_probe_slot1" "child acquired a slot we hold"
else
    record_pass "deny.flock_probe_slot1"
fi
CHILD_HELD="$(LIB="$LIB" DIR="$SLOTS" PUSH_GATE_MAX_CONCURRENT=2 PUSH_GATE_MAX_WAIT_SECONDS=0 PUSH_GATE_POLL_SECONDS=1 \
    bash -c '. "$LIB"; if push_gate_acquire_slot "$DIR" z holder-C; then echo ACQUIRED; else echo DENIED; fi')"
assert_eq "deny.lib_from_child_zero_wait" "$CHILD_HELD" "DENIED"

# ---------------- all slots busy: waits, prints diagnostics, then times out with rc 1 ----------------
# The child exits with push_gate_acquire_slot's own status so CHILD_RC asserts
# the LIBRARY's return code, not the child shell's last echo.
START="$(date +%s)"
CHILD_OUT="$(LIB="$LIB" DIR="$SLOTS" PUSH_GATE_MAX_CONCURRENT=2 PUSH_GATE_MAX_WAIT_SECONDS=2 PUSH_GATE_POLL_SECONDS=1 \
    bash -c '. "$LIB"; push_gate_acquire_slot "$DIR" z holder-D; rc=$?; if [[ "$rc" -eq 0 ]]; then echo ACQUIRED; else echo "DENIED rc=$rc"; fi; exit "$rc"' 2>&1)"
CHILD_RC=$?
ELAPSED=$(( $(date +%s) - START ))
assert_contains "wait.busy_message_immediate" "$CHILD_OUT" "busy"
assert_contains "wait.reports_holder_a"        "$CHILD_OUT" "holder-A"
assert_contains "wait.timeout_message"         "$CHILD_OUT" "timed out"
assert_contains "wait.eventually_denied"       "$CHILD_OUT" "DENIED"
assert_true     "wait.elapsed_at_least_bound"  test "$ELAPSED" -ge 2
assert_eq       "wait.library_returns_1_on_timeout" "$CHILD_RC" "1"

# ---------------- release -> reacquire ----------------
push_gate_release_slot "$FD0"
if flock -n "$SLOTS/slot-0.lock" -c 'exit 0'; then
    record_pass "release.flock_probe_succeeds"
else
    record_fail "release.flock_probe_succeeds" "could not acquire slot-0 after release"
fi
CHILD_FREE="$(LIB="$LIB" DIR="$SLOTS" PUSH_GATE_MAX_CONCURRENT=2 PUSH_GATE_MAX_WAIT_SECONDS=1 PUSH_GATE_POLL_SECONDS=1 \
    bash -c '. "$LIB"; if push_gate_acquire_slot "$DIR" z holder-E; then echo ACQUIRED; else echo DENIED; fi')"
assert_eq "release.lib_from_child" "$CHILD_FREE" "ACQUIRED"
push_gate_release_slot "$FD1"

# ---------------- FD inheritance: a live detached descendant must not pin a slot ----------------
# scripts/test-local-parallel closes the gate FD before the fan-out so jobs
# never inherit it (asserted statically below). This asserts the library half
# of that contract: releasing unlocks the open-file-description itself, so
# even a descendant that did inherit a copy — a daemonized tmux server, a
# dolt sql-server, an escaped `gc` — stops holding the slot the moment the
# gate releases it.
INHERIT_SLOTS="$WORK/inherit-slots"
FD_INH=""
if push_gate_acquire_slot "$INHERIT_SLOTS" FD_INH "holder-inherit"; then
    if command -v setsid >/dev/null 2>&1; then
        setsid bash -c 'sleep 3' &
    else
        bash -c 'sleep 3' &
    fi
    INHERIT_CHILD=$!
    push_gate_release_slot "$FD_INH"
    if flock -n "$INHERIT_SLOTS/slot-0.lock" -c 'exit 0'; then
        record_pass "inherit.detached_descendant_does_not_pin_slot"
    else
        record_fail "inherit.detached_descendant_does_not_pin_slot" \
            "slot still held after release while a detached descendant is alive"
    fi
    kill "$INHERIT_CHILD" 2>/dev/null || true
    wait "$INHERIT_CHILD" 2>/dev/null || true
else
    record_fail "inherit.detached_descendant_does_not_pin_slot" "could not acquire a slot to set up the case"
fi

# ---------------- describe: a held slot whose recorded PID is gone is flagged ----------------
# Hold a slot for real, then overwrite its holder line with a PID that cannot
# exist — the exact shape a leaked descendant leaves behind: lock genuinely
# held, recorded holder long gone.
DEAD_SLOTS="$WORK/dead-slots"
FD_DEAD=""
if push_gate_acquire_slot "$DEAD_SLOTS" FD_DEAD "holder-dead"; then
    printf '%s %s %s %s\n' "999999999" "1970-01-01T00:00:00Z" "holder-dead" "somehost" >"$DEAD_SLOTS/slot-0.lock"
    DEAD_OUT="$(push_gate_describe_slots "$DEAD_SLOTS" 1)"
    assert_contains "describe.flags_dead_holder" "$DEAD_OUT" "holder pid dead"
    push_gate_release_slot "$FD_DEAD"
else
    record_fail "describe.flags_dead_holder" "could not acquire a slot to set up the case"
fi

# ---------------- missing flock(1): degrade best-effort, never block ----------------
# PATH is stripped of every directory so `command -v flock` fails; the library
# must warn and return 0 with an empty FD rather than burn the wait bound.
mkdir -p "$WORK/empty-bin"
# shellcheck disable=SC2016  # $? and $z are the child shell's, evaluated there
NOFLOCK_OUT="$(LIB="$LIB" DIR="$WORK/noflock-slots" PATH="$WORK/empty-bin" \
    PUSH_GATE_MAX_CONCURRENT=1 PUSH_GATE_MAX_WAIT_SECONDS=0 PUSH_GATE_POLL_SECONDS=1 \
    "$BASH" -c '. "$LIB"; z=preset; push_gate_acquire_slot "$DIR" z holder-F; echo "rc=$? fd=[$z]"' 2>&1)"
assert_contains "no_flock.warns_and_names_flock" "$NOFLOCK_OUT" "flock(1) not found"
assert_contains "no_flock.returns_zero_empty_fd" "$NOFLOCK_OUT" "rc=0 fd=[]"

# ---------------- malformed tunables fall back to their documented defaults ----------------
# Each bad value must be rejected by name and replaced, never fed to
# arithmetic (`-1`, `abc`) or turned into a busy loop / zero-slot sweep (`0`).
for bad_case in "empty:" "zero:0" "negative:-1" "nonnumeric:abc"; do
    bad_name="${bad_case%%:*}"
    bad_val="${bad_case#*:}"
    TUNE_OUT="$(LIB="$LIB" DIR="$WORK/tunables-max-$bad_name" \
        PUSH_GATE_MAX_CONCURRENT="$bad_val" PUSH_GATE_MAX_WAIT_SECONDS=1 PUSH_GATE_POLL_SECONDS=1 \
        bash -c '. "$LIB"; push_gate_acquire_slot "$DIR" z holder-T; echo "rc=$?"' 2>&1)"
    assert_contains "tunables.max_concurrent_${bad_name}_still_acquires" "$TUNE_OUT" "rc=0"
    # An empty value is already covered by the `${VAR:-default}` expansion, so
    # only the values that actually reach validation emit the warning.
    if [[ -n "$bad_val" ]]; then
        assert_contains "tunables.max_concurrent_${bad_name}_warns_by_name" "$TUNE_OUT" "PUSH_GATE_MAX_CONCURRENT"
    fi
done
for bad_tunable in PUSH_GATE_MAX_WAIT_SECONDS PUSH_GATE_POLL_SECONDS; do
    TUNE_OUT="$(LIB="$LIB" DIR="$WORK/tunables-$bad_tunable" BAD="$bad_tunable" \
        bash -c 'export "$BAD=abc"; . "$LIB"; push_gate_acquire_slot "$DIR" z holder-T; echo "rc=$?"' 2>&1)"
    assert_contains "tunables.${bad_tunable}_warns_by_name" "$TUNE_OUT" "$bad_tunable"
    assert_contains "tunables.${bad_tunable}_still_acquires" "$TUNE_OUT" "rc=0"
done

# ---------------- escape hatch: GC_PUSH_GATE_NO_CAP bypasses the cap entirely ----------------
NOCAP_OUT="$(LIB="$LIB" DIR="$SLOTS" GC_PUSH_GATE_NO_CAP=1 PUSH_GATE_MAX_CONCURRENT=1 PUSH_GATE_MAX_WAIT_SECONDS=0 \
    bash -c '. "$LIB"; if push_gate_acquire_slot "$DIR" z; then echo ACQUIRED; else echo DENIED; fi')"
assert_eq "no_cap.bypasses_full_slots" "$NOCAP_OUT" "ACQUIRED"

# ---------------- city-root resolution: env short-circuit ----------------
CITY_ENV="$WORK/city-env"
mkdir -p "$CITY_ENV"
: >"$CITY_ENV/city.toml"
RESOLVED_ENV="$(GC_CITY_PATH="$CITY_ENV" GC_CITY="" GC_CITY_ROOT="" push_gate_city_root)"
assert_eq "city_root.env_short_circuit" "$RESOLVED_ENV" "$CITY_ENV"

# An env var pointing at a directory with neither city.toml nor .gc/ must NOT
# be trusted verbatim — it should fall through to walk-up instead of
# returning garbage. Bounded entirely inside $WORK (HOME=$WORK as the
# ceiling) so this can't accidentally pick up a real city.toml somewhere in
# the ambient filesystem's actual ancestry.
BOGUS="$WORK/not-a-city"
mkdir -p "$BOGUS/nested"
if RESOLVED_BOGUS="$(cd "$BOGUS/nested" && GC_CITY_PATH="$BOGUS" GC_CITY="" GC_CITY_ROOT="" HOME="$WORK" push_gate_city_root)"; then
    assert_true "city_root.rejects_unvalidated_env" test "$RESOLVED_BOGUS" != "$BOGUS"
else
    record_pass "city_root.rejects_unvalidated_env"
fi

# ---------------- city-root resolution: walk-up discovery ----------------
CITY_WALK="$WORK/city-walk"
mkdir -p "$CITY_WALK/rigs/proj/sub"
: >"$CITY_WALK/city.toml"
RESOLVED_WALK="$(cd "$CITY_WALK/rigs/proj/sub" && GC_CITY_PATH="" GC_CITY="" GC_CITY_ROOT="" HOME="$WORK/unrelated-home" push_gate_city_root)"
assert_eq "city_root.walk_up_finds_ancestor" "$RESOLVED_WALK" "$CITY_WALK"

# ---------------- city-root resolution: the legacy .gc/ fallback is bounded ----------------
# push_gate_city_root is a port of cmd/gc/city_discovery.go's
# findCityWithOptions, whose legacy `.gc/`-only fallback is bounded three ways:
# it is skipped at a ceiling directory ($HOME, $TMPDIR, $GC_CEILING_DIRECTORIES),
# and it ignores both the supervisor's global runtime root ($GC_HOME, default
# $HOME/.gc) and any `.gc/` under an ancestor of $TMPDIR.
#
# Those bounds are what make the slot pool city-wide. An agent whose walk-up
# adopts an unrelated `.gc/` as its city root gets a *private* pool and runs its
# own PUSH_GATE_MAX_CONCURRENT heavy suites on the same box, so the cap silently
# stops capping. Observed live on a 24-core host (ga-8i9z): a real gate run held
# ~/.gc/gate-slots/slot-0.lock — $GC_HOME, not a city — while the true city pool
# was separately full.

# $HOME is a ceiling, so a `.gc/` sitting in it must never become the city root.
FAKE_HOME="$WORK/fake-home"
mkdir -p "$FAKE_HOME/.gc" "$FAKE_HOME/proj"
(cd "$FAKE_HOME/proj" && git init -q .) 2>/dev/null || true
if RESOLVED_HOME="$(cd "$FAKE_HOME/proj" && GC_CITY_PATH="" GC_CITY="" GC_CITY_ROOT="" GC_HOME="" HOME="$FAKE_HOME" push_gate_city_root)"; then
    assert_true "city_root.ignores_home_dot_gc" test "$RESOLVED_HOME" != "$FAKE_HOME"
else
    record_pass "city_root.ignores_home_dot_gc"
fi

# ...and the slot pool must fall back to repo-relative rather than silently
# becoming a private per-$HOME pool.
SLOTS_HOME="$(cd "$FAKE_HOME/proj" && GC_CITY_PATH="" GC_CITY="" GC_CITY_ROOT="" GC_HOME="" HOME="$FAKE_HOME" push_gate_slots_dir)"
assert_true "slots_dir.never_under_home_dot_gc" test "$SLOTS_HOME" != "$FAKE_HOME/.gc/gate-slots"

# A ceiling stops the *legacy* fallback, not an explicit city: findCity checks
# HasCityConfig before the ceiling break, so a real city.toml at $HOME wins.
HOME_CITY="$WORK/home-city"
mkdir -p "$HOME_CITY/proj"
: >"$HOME_CITY/city.toml"
RESOLVED_HOME_CITY="$(cd "$HOME_CITY/proj" && GC_CITY_PATH="" GC_CITY="" GC_CITY_ROOT="" GC_HOME="" HOME="$HOME_CITY" push_gate_city_root)"
assert_eq "city_root.city_toml_wins_at_ceiling" "$RESOLVED_HOME_CITY" "$HOME_CITY"

# $GC_HOME is the supervisor's global runtime root. It is a `.gc/` directory by
# construction and is never a city, even when it sits below the $HOME ceiling.
SUPERVISOR="$WORK/supervisor"
mkdir -p "$SUPERVISOR/.gc" "$SUPERVISOR/proj"
if RESOLVED_GC_HOME="$(cd "$SUPERVISOR/proj" && GC_CITY_PATH="" GC_CITY="" GC_CITY_ROOT="" GC_HOME="$SUPERVISOR/.gc" HOME="$WORK/unrelated-home" push_gate_city_root)"; then
    assert_true "city_root.ignores_gc_home_runtime_root" test "$RESOLVED_GC_HOME" != "$SUPERVISOR"
else
    record_pass "city_root.ignores_gc_home_runtime_root"
fi

# A `.gc/` under an ancestor of $TMPDIR is build/runtime scratch, not a city.
# Reached only from a sibling path, since $TMPDIR itself is a ceiling.
TMPROOT="$WORK/tmproot"
mkdir -p "$TMPROOT/.gc" "$TMPROOT/other/repo" "$TMPROOT/gct-prefixed"
if RESOLVED_TMP="$(cd "$TMPROOT/other/repo" && GC_CITY_PATH="" GC_CITY="" GC_CITY_ROOT="" GC_HOME="" HOME="$WORK/unrelated-home" TMPDIR="$TMPROOT/gct-prefixed" push_gate_city_root)"; then
    assert_true "city_root.ignores_tmpdir_ancestor_dot_gc" test "$RESOLVED_TMP" != "$TMPROOT"
else
    record_pass "city_root.ignores_tmpdir_ancestor_dot_gc"
fi

# The ancestor walk stops before the filesystem root, matching the Go's
# `dir != filepath.Dir(dir)` guard. Ignoring `/.gc` would make bash refuse a
# legacy root the Go accepts, and a bash/Go disagreement about the city root
# splits the slot pool — the very failure this bounding exists to prevent.
IGNORED_ROOTS="$(TMPDIR=/tmp _push_gate_ignored_runtime_roots)"
assert_contains "ignored_roots.includes_tmpdir_ancestor" "$IGNORED_ROOTS" "/tmp/.gc"
assert_false "ignored_roots.stops_before_filesystem_root" \
    grep -qx '/\.gc' <<<"$IGNORED_ROOTS"

# ---------------- slots dir: derives from city root, falls back to repo-relative ----------------
SLOTS_FROM_CITY="$(GC_CITY_PATH="$CITY_ENV" GC_CITY="" GC_CITY_ROOT="" push_gate_slots_dir)"
assert_eq "slots_dir.under_city_root" "$SLOTS_FROM_CITY" "$CITY_ENV/.gc/gate-slots"

NOCITY="$WORK/no-city-repo"
mkdir -p "$NOCITY"
(cd "$NOCITY" && git init -q .) 2>/dev/null || true
# HOME=$WORK, not a sibling of it: HOME is the walk-up ceiling, so pointing it
# at a directory outside $NOCITY's ancestry lets the walk escape the fixture
# and inspect the real filesystem above $TMPDIR. This assertion only held on
# hosts where no ancestor of $TMPDIR happens to contain a .gc directory — on a
# Gas City host with /tmp/.gc (a build cache, not a city) the walk resolved
# /tmp as a legacy city root and the fallback never ran.
SLOTS_FALLBACK="$(cd "$NOCITY" && GC_CITY_PATH="" GC_CITY="" GC_CITY_ROOT="" HOME="$WORK" push_gate_slots_dir)"
assert_eq "slots_dir.falls_back_to_repo_relative" "$SLOTS_FALLBACK" "$NOCITY/.git/gate-slots"

# A linked worktree's .git is a FILE, so a per-worktree `.git/gate-slots` is not
# merely a split pool — it is an uncreatable one, and the gate can never be
# acquired from there. The fallback must resolve through the repository's common
# git dir so every linked worktree of a repo shares the main repo's pool.
# HOME=$WORK for the same reason as the fallback assertion above: the walk-up
# ceiling has to stay inside the fixture.
LINKED_REPO="$WORK/linked-repo"
LINKED_A="$WORK/linked-a"
LINKED_B="$WORK/linked-b"
mkdir -p "$LINKED_REPO"
git -C "$LINKED_REPO" init -q
git -C "$LINKED_REPO" config user.name "Push Gate Test"
git -C "$LINKED_REPO" config user.email "push-gate-test@example.invalid"
: >"$LINKED_REPO/tracked"
git -C "$LINKED_REPO" add tracked
git -C "$LINKED_REPO" commit -qm "seed linked worktree fixture"
git -C "$LINKED_REPO" worktree add -q --detach "$LINKED_A"
git -C "$LINKED_REPO" worktree add -q --detach "$LINKED_B"

SLOTS_LINKED_A="$(cd "$LINKED_A" && GC_CITY_PATH="" GC_CITY="" GC_CITY_ROOT="" HOME="$WORK" push_gate_slots_dir)"
SLOTS_LINKED_B="$(cd "$LINKED_B" && GC_CITY_PATH="" GC_CITY="" GC_CITY_ROOT="" HOME="$WORK" push_gate_slots_dir)"
LINKED_COMMON="$(cd "$LINKED_REPO/.git" && pwd -P)"
SLOTS_LINKED_A_N="$(cd "$(dirname "$SLOTS_LINKED_A")" && pwd -P)/gate-slots"
assert_eq "slots_dir.linked_worktree_uses_common_git_dir" "$SLOTS_LINKED_A_N" "$LINKED_COMMON/gate-slots"
assert_eq "slots_dir.linked_worktrees_share_slots" "$SLOTS_LINKED_B" "$SLOTS_LINKED_A"

FD_LINKED=""
if push_gate_acquire_slot "$SLOTS_LINKED_A" FD_LINKED "linked-holder"; then
    record_pass "slots_dir.linked_worktree_can_acquire"
    push_gate_release_slot "$FD_LINKED"
else
    record_fail "slots_dir.linked_worktree_can_acquire" "resolved slot directory is not usable: $SLOTS_LINKED_A"
fi

# ---------------- static wiring assertions against test-local-parallel ----------------
assert_true "wiring.sources_lib"   grep -q 'push-gate-lock-lib.sh' "$LOCAL_PARALLEL"
assert_true "wiring.calls_acquire" grep -q 'push_gate_acquire_slot' "$LOCAL_PARALLEL"
assert_true "wiring.has_override"  grep -q 'GC_PUSH_GATE_NO_CAP'   "$LOCAL_PARALLEL"

acq_line="$(grep -n 'push_gate_acquire_slot ' "$LOCAL_PARALLEL" | head -1 | cut -d: -f1)"
if [[ -n "$acq_line" ]]; then
    LOSER_BLOCK="$(sed -n "${acq_line},$((acq_line + 10))p" "$LOCAL_PARALLEL")"
    assert_contains "wiring.timeout_exits_75" "$LOSER_BLOCK" "exit 75"
else
    record_fail "wiring.timeout_exits_75" "no push_gate_acquire_slot call found in $LOCAL_PARALLEL"
fi
# Must be the release call wired to an EXIT trap specifically — not just any
# trap and any release call existing independently somewhere in the file
# (e.g. an unrelated per-job cleanup trap for a temp dir).
assert_true "wiring.releases_slot_on_exit" grep -qE 'trap .*push_gate_release_slot.*EXIT' "$LOCAL_PARALLEL"

# The gate FD must be closed BEFORE the job fan-out, or every job — and every
# daemon a job leaks — inherits a copy and can pin the slot past this
# invocation's death. Line ordering is the assertion: a close that lands after
# the fan-out is worthless.
close_line="$(grep -nE 'exec \$\{gate_fd\}>&-' "$LOCAL_PARALLEL" | head -1 | cut -d: -f1)"
fanout_line="$(grep -n 'xargs -0' "$LOCAL_PARALLEL" | head -1 | cut -d: -f1)"
if [[ -n "$close_line" && -n "$fanout_line" && "$close_line" -lt "$fanout_line" ]]; then
    record_pass "wiring.closes_gate_fd_before_fanout"
else
    record_fail "wiring.closes_gate_fd_before_fanout" \
        "close at line '${close_line:-none}', fan-out at line '${fanout_line:-none}'"
fi

# ---------------- nesting guard (ga-spc) ----------------
# The gated targets shell out to each other: test-local-parallel's `full` mode
# runs `make test-productmetrics-testhook`, which re-enters
# scripts/go-test-observable. The child must inherit "already gated" rather
# than take a second slot — otherwise one logical invocation double-counts
# against the cap, and once every slot is held by such a parent each child
# blocks against its own parent until the wait bound expires.
NEST_SLOTS="$WORK/nest-slots"
NEST_FD=""
(
    PUSH_GATE_MAX_CONCURRENT=1
    export PUSH_GATE_MAX_CONCURRENT
    unset GC_PUSH_GATE_HELD
    push_gate_acquire_slot "$NEST_SLOTS" NEST_FD "nesting-parent" >/dev/null 2>&1 || exit 1
    [[ "${GC_PUSH_GATE_HELD:-}" == "1" ]] || exit 2
    exit 0
)
case "$?" in
    0) record_pass "nesting.acquire_exports_held_marker" ;;
    1) record_fail "nesting.acquire_exports_held_marker" "parent could not acquire a slot" ;;
    *) record_fail "nesting.acquire_exports_held_marker" "GC_PUSH_GATE_HELD was not exported after acquire" ;;
esac

# The marker must not be set by merely sourcing the library, or every runner
# would believe it was already gated and the cap would never apply at all.
assert_true "nesting.marker_unset_before_acquire" \
    env -u GC_PUSH_GATE_HELD bash -c ". \"\$1\"; [ -z \"\${GC_PUSH_GATE_HELD:-}\" ]" _ "$LIB"

# ---------------- static wiring assertions against go-test-observable ----------------
# The other funnel to `go test`: `make test`, `make test-mac`,
# `make test-cmd-gc-process` and `make test-productmetrics-testhook` all reach
# the product through this script, and the unsharded ./... sweep it runs is the
# heaviest single job on a shared host.
OBSERVABLE="$TEST_DIR/go-test-observable"
assert_true "observable.sources_lib"    grep -q 'push-gate-lock-lib.sh' "$OBSERVABLE"
assert_true "observable.calls_acquire"  grep -q 'push_gate_acquire_slot' "$OBSERVABLE"
assert_true "observable.honors_nesting" grep -q 'GC_PUSH_GATE_HELD'     "$OBSERVABLE"
assert_true "observable.releases_slot_on_exit" \
    grep -qE 'trap .*push_gate_release_slot.*EXIT' "$OBSERVABLE"

obs_acq_line="$(grep -n 'push_gate_acquire_slot ' "$OBSERVABLE" | head -1 | cut -d: -f1)"
if [[ -n "$obs_acq_line" ]]; then
    OBS_LOSER_BLOCK="$(sed -n "${obs_acq_line},$((obs_acq_line + 6))p" "$OBSERVABLE")"
    assert_contains "observable.timeout_exits_75" "$OBS_LOSER_BLOCK" "exit 75"
else
    record_fail "observable.timeout_exits_75" "no push_gate_acquire_slot call found in $OBSERVABLE"
fi

# Gating must key on a resolvable city root, not on push_gate_slots_dir's
# repo-relative fallback: outside a city there is no fleet to contend with, and
# queueing a CI runner behind a bound it does not need would stall the one
# caller that was never the problem.
assert_true "observable.gates_only_inside_a_city" grep -q 'push_gate_city_root' "$OBSERVABLE"

# Same FD-inheritance hazard as the fan-out, same ordering assertion: the slot
# descriptor must be closed before the product `go test` is exec'd.
obs_close_line="$(grep -nE 'exec \$\{gate_fd\}>&-' "$OBSERVABLE" | head -1 | cut -d: -f1)"
obs_exec_line="$(grep -n 'exec go test -json' "$OBSERVABLE" | head -1 | cut -d: -f1)"
if [[ -n "$obs_close_line" && -n "$obs_exec_line" && "$obs_close_line" -lt "$obs_exec_line" ]]; then
    record_pass "observable.closes_gate_fd_before_go_test"
else
    record_fail "observable.closes_gate_fd_before_go_test" \
        "close at line '${obs_close_line:-none}', exec at line '${obs_exec_line:-none}'"
fi

# A signal-killed sweep is not a test result. Reported as a bare
# "FAIL status=143" it reads as a real regression, and a caller piping through
# `tail` sees only the pipe's exit 0 and calls a killed sweep a pass.
assert_true "observable.names_signal_deaths" grep -q 'KILLED by signal' "$OBSERVABLE"

# Concurrent sweeps must not produce mutually indistinguishable JSON logs in
# the shared temp dir — that is how one agent reads another's failures as its
# own.
assert_true "observable.log_name_identifies_owner" \
    grep -qE 'gascity-\$\{safe_name\}-\$\{safe_owner\}-\$\$' "$OBSERVABLE"

echo
echo "push-gate-lock tests: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
