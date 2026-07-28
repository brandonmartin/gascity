#!/usr/bin/env bash
#
# Self-test for scripts/lib/verified-tree.sh — the push-gate dedup that lets a
# suite's verdict be read by the next gate over the same tree (ga-4rr4).
#
# Driven as a plain shell job from scripts/test-local-parallel rather than
# through a `go test` trampoline. A trampoline's exec.Command would itself add
# tracked subprocess call sites to internal/testpolicy/resourcecensus, whose
# scope=all audit row is pinned to an exact count and fails on any change --
# growth or shrinkage alike, with no per-file exemption. Running as a shell job
# sidesteps that ratchet instead of bumping it, the same way
# scripts/test-push-gate-lock.sh does.
#
# The hook-level half of this contract (does pre-push actually skip?) lives in
# scripts/githooks_pre_push_verified_tree_test.go, which drives the real hook
# through existing fixture helpers and so adds no new call sites.

set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$TEST_DIR/.." && pwd)"
LIB="$TEST_DIR/lib/verified-tree.sh"
RUNNER="$TEST_DIR/test-local-parallel"

pass=0; fail=0
record_pass() { echo "  ok   $1"; pass=$((pass + 1)); }
record_fail() { echo "  FAIL $1 — $2"; fail=$((fail + 1)); }
assert_true()  { if "${@:2}"; then record_pass "$1"; else record_fail "$1" "expected success"; fi; }
assert_false() { if "${@:2}"; then record_fail "$1" "expected failure"; else record_pass "$1"; fi; }
assert_eq() {
    local name="$1" got="$2" want="$3"
    if [[ "$got" == "$want" ]]; then record_pass "$name"
    else record_fail "$name" "got '$got', want '$want'"; fi
}

WORKDIRS=()
cleanup() {
    local d
    for d in "${WORKDIRS[@]:-}"; do
        [[ -n "$d" && -d "$d" ]] || continue
        chmod -R u+w "$d" 2>/dev/null || true
        rm -rf "$d"
    done
}
trap cleanup EXIT

# new_repo: throwaway git repo with one commit and a clean worktree. The
# library's contract is expressed in real git state, so these drive real repos
# rather than stubbing git.
new_repo() {
    local d
    d="$(mktemp -d -p "${TMPDIR:-/var/tmp}" gc-vt.XXXXXX)" || return 1
    WORKDIRS+=("$d")
    git -C "$d" init -q -b main >/dev/null 2>&1
    git -C "$d" config user.email test@example.com
    git -C "$d" config user.name test
    echo fixture > "$d/README.md"
    git -C "$d" add -A
    git -C "$d" commit -q --no-verify -m base
    printf '%s' "$d"
}

# in_repo <dir> <body>: run body with the library sourced, inside the repo.
# PUSH_GATE_* are neutralized so an operator's environment cannot retune any
# freshness assertion below.
in_repo() {
    local dir="$1" body="$2"
    ( cd "$dir" && env -u PUSH_GATE_VERIFIED_TTL_SECONDS -u PUSH_GATE_IGNORE_VERIFIED \
        bash -c ". '$LIB'
$body" )
}

commit_file() {
    local dir="$1" name="$2"
    echo "package fixture" > "$dir/$name"
    git -C "$dir" add -A
    git -C "$dir" commit -q --no-verify -m "add $name"
}

echo "verified-tree self-test"

# --- the dedup this library exists for -------------------------------------
repo="$(new_repo)"
assert_false "fresh.before_any_run"  in_repo "$repo" 'gc_verified_tree_is_fresh fast'
in_repo "$repo" 'gc_verified_tree_record fast' >/dev/null
assert_true  "fresh.after_record"    in_repo "$repo" 'gc_verified_tree_is_fresh fast'

# A cheap suite must not vouch for an expensive one.
assert_false "fresh.other_suite"     in_repo "$repo" 'gc_verified_tree_is_fresh full'

# Content-addressed: a new commit invalidates the marker without anything
# having to invalidate it.
commit_file "$repo" added.go
assert_false "fresh.after_new_commit" in_repo "$repo" 'gc_verified_tree_is_fresh fast'

# --- clean trees only ------------------------------------------------------
# Uncommitted edits are not in HEAD's tree hash. Keying a verdict on one would
# let untested content ride out on a marker earned by different content.
repo="$(new_repo)"
in_repo "$repo" 'gc_verified_tree_record fast' >/dev/null
echo "package fixture" > "$repo/dirty.go"
assert_false "dirty.has_no_tree_id"  in_repo "$repo" 'gc_verified_tree_id'
assert_false "dirty.never_fresh"     in_repo "$repo" 'gc_verified_tree_is_fresh fast'

# Record side of the same property: if the content moved while the suite ran,
# the verdict describes neither tree.
repo="$(new_repo)"
in_repo "$repo" 'gc_verified_tree_record fast aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' >/dev/null
assert_false "record.skips_on_tree_change" in_repo "$repo" 'gc_verified_tree_is_fresh fast'

# --- time bound ------------------------------------------------------------
repo="$(new_repo)"
in_repo "$repo" 'gc_verified_tree_record fast' >/dev/null
marker="$(in_repo "$repo" 'gc_verified_tree_marker fast "$(gc_verified_tree_id)"')"
touch -d "2020-01-01" "$marker"
assert_false "ttl.expired_marker_ignored" in_repo "$repo" 'gc_verified_tree_is_fresh fast'
assert_true  "ttl.explicit_long_ttl_honors_it" \
    env PUSH_GATE_VERIFIED_TTL_SECONDS=999999999 bash -c "cd '$repo' && . '$LIB' && gc_verified_tree_is_fresh fast"

# Zero TTL is an off switch that needs no state deleted.
repo="$(new_repo)"
in_repo "$repo" 'gc_verified_tree_record fast' >/dev/null
assert_false "ttl.zero_disables_reuse" \
    env PUSH_GATE_VERIFIED_TTL_SECONDS=0 bash -c "cd '$repo' && . '$LIB' && gc_verified_tree_is_fresh fast"

# Per-invocation escape hatch for chasing a suspected flake.
assert_false "override.ignore_verified" \
    env PUSH_GATE_IGNORE_VERIFIED=1 bash -c "cd '$repo' && . '$LIB' && gc_verified_tree_is_fresh fast"

# --- markers stay out of the content they vouch for ------------------------
# A cache file inside the worktree would show up as untracked — itself a reason
# to refuse the tree — and could be swept into a commit by `git add -A`.
repo="$(new_repo)"
in_repo "$repo" 'gc_verified_tree_record fast' >/dev/null
marker="$(in_repo "$repo" 'gc_verified_tree_marker fast "$(gc_verified_tree_id)"')"
gitdir="$(git -C "$repo" rev-parse --absolute-git-dir)"
case "$marker" in
  "$gitdir"/*) record_pass "marker.lives_under_git_dir" ;;
  *) record_fail "marker.lives_under_git_dir" "marker '$marker' is not under '$gitdir'" ;;
esac
assert_eq "marker.worktree_stays_clean" "$(git -C "$repo" status --porcelain)" ""

# --- pruning ---------------------------------------------------------------
# Every distinct tree ever verified leaves a file behind; without pruning the
# cache grows for the life of the clone.
repo="$(new_repo)"
in_repo "$repo" 'gc_verified_tree_record fast' >/dev/null
cachedir="$(in_repo "$repo" 'gc_verified_tree_dir')"
stale="$cachedir/fast.bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
echo stale > "$stale"
touch -d "2020-01-01" "$stale"
commit_file "$repo" added.go
in_repo "$repo" 'gc_verified_tree_record fast' >/dev/null
assert_false "prune.removes_expired" test -e "$stale"
assert_true  "prune.keeps_just_recorded" in_repo "$repo" 'gc_verified_tree_is_fresh fast'

# --- failure posture: a cache write must never fail a green suite -----------
# The runner calls record under `set -e` immediately after a suite passed, so
# any unguarded command in the cache path turns a green run red — the ga-8qmy
# misreading, reintroduced through a cache write.
repo="$(new_repo)"
in_repo "$repo" 'gc_verified_tree_record fast' >/dev/null
cachedir="$(in_repo "$repo" 'gc_verified_tree_dir')"
expired="$cachedir/fast.dddddddddddddddddddddddddddddddddddddddd"
echo expired > "$expired"
touch -d "2020-01-01" "$expired"
if [[ "$(id -u)" -eq 0 ]]; then
    echo "  skip verified-tree.unwritable_cache (running as root bypasses directory permissions)"
else
    chmod 500 "$cachedir"
    commit_file "$repo" added.go
    out="$(in_repo "$repo" 'set -euo pipefail
gc_verified_tree_record fast
echo reached-the-end' 2>/dev/null)"
    assert_eq "record.survives_unwritable_cache" "$out" "reached-the-end"
    chmod 700 "$cachedir"
fi

# Outside a git repo there is nothing to name, and still nothing to fail on.
outside="$(mktemp -d -p "${TMPDIR:-/var/tmp}" gc-vt-bare.XXXXXX)"
WORKDIRS+=("$outside")
out="$(cd "$outside" && GIT_CEILING_DIRECTORIES="$(dirname "$outside")" bash -c "set -euo pipefail
. '$LIB'
gc_verified_tree_record fast
echo reached-the-end" 2>/dev/null)"
assert_eq "record.survives_outside_a_repo" "$out" "reached-the-end"

# --- runner wiring ---------------------------------------------------------
# The runner is the only place that learns a suite passed. If it stops
# recording, the pre-push gate silently goes back to running every suite twice
# and nothing else here would fail.
assert_true "wiring.runner_sources_lib"   grep -q 'lib/verified-tree.sh' "$RUNNER"
assert_true "wiring.runner_records"       grep -q 'gc_verified_tree_record' "$RUNNER"

green_line="$(grep -n 'All \${mode} jobs passed' "$RUNNER" | head -1 | cut -d: -f1)"
record_line="$(grep -n 'gc_verified_tree_record' "$RUNNER" | head -1 | cut -d: -f1)"
fail_line="$(grep -n 'gc_report_suite_outcome' "$RUNNER" | head -1 | cut -d: -f1)"
if [[ -n "$green_line" && -n "$record_line" && "$record_line" -lt "$green_line" ]]; then
    record_pass "wiring.records_on_green_path"
else
    record_fail "wiring.records_on_green_path" "record at '${record_line:-none}', green epilogue at '${green_line:-none}'"
fi
if [[ -z "$fail_line" || "$record_line" -lt "$fail_line" ]]; then
    record_pass "wiring.never_records_on_red"
else
    record_fail "wiring.never_records_on_red" "record at '$record_line' comes after the failure epilogue at '$fail_line'"
fi

# pre-push must consult the verdict, and must do so *after* the beads chain and
# the ownership guard: only the duplicated suite is skipped, never the guard.
HOOK="$REPO_ROOT/.githooks/pre-push"
assert_true "wiring.hook_consults_verdict" grep -q 'gc_verified_tree_is_fresh' "$HOOK"
guard_line="$(grep -n 'assert_bead_still_claimed' "$HOOK" | head -1 | cut -d: -f1)"
fresh_line="$(grep -n 'gc_verified_tree_is_fresh' "$HOOK" | head -1 | cut -d: -f1)"
if [[ -n "$guard_line" && -n "$fresh_line" && "$guard_line" -lt "$fresh_line" ]]; then
    record_pass "wiring.dedup_below_ownership_guard"
else
    record_fail "wiring.dedup_below_ownership_guard" "guard at '${guard_line:-none}', dedup at '${fresh_line:-none}'"
fi

echo
echo "verified-tree tests: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
