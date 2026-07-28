# shellcheck shell=bash
#
# Shared record of "this exact content already passed this suite", for the
# push-gate layers that would otherwise ask the same question twice.
#
# The refinery's run-tests step and .githooks/pre-push both run the fast suite,
# and on a merge they run it over the *same* tree: the refinery verifies the
# merged result, then pushes it, and the push gate verifies the merged result
# again. Measured on one branch (ga-4rr4) that was ~12min followed by ~16min,
# roughly half the merge's wall clock spent re-deciding a settled question,
# multiplied by every branch in the queue.
#
# Neither gate can simply be deleted. The refinery needs its own verdict
# *before* pushing, because that is what lets it tell "this branch is bad"
# (reject to the pool) from "the push flaked" (retry) — a distinction this rig
# depends on (ga-2byy, ga-lsm4, ga-b60l, ga-xiaj). So instead of dropping a
# gate, the suite that runs first leaves a note, and the gate that runs second
# reads it.
#
# What a marker asserts, exactly: the tree object named by this file passed the
# named suite on this machine, at this file's mtime. Three properties keep that
# honest:
#
#   Content-addressed. The key is HEAD's tree hash, so a marker cannot outlive
#   the content it vouches for — one new commit and it simply does not match.
#
#   Clean trees only. Uncommitted edits are not in HEAD's tree hash, so a dirty
#   worktree has no name here at all: it is neither recorded nor honored.
#   Without this, untested edits would ride out on a marker earned by other
#   content.
#
#   Time-bounded. A tree hash pins the content but not the toolchain or the
#   host around it, so a verdict expires (PUSH_GATE_VERIFIED_TTL_SECONDS).
#
# Markers live under the common git dir, never in the worktree: a cache file
# inside the tree would show up as untracked — which is itself a reason to
# refuse the tree — and could be swept into a commit by `git add -A`.
#
# Environment:
#   PUSH_GATE_VERIFIED_TTL_SECONDS  seconds a verdict stays valid (default
#                                   below). 0 disables reuse without requiring
#                                   any state to be deleted.
#   PUSH_GATE_IGNORE_VERIFIED       non-empty forces the real run for one
#                                   invocation, for chasing a suspected flake.

# gc_verified_tree_default_ttl_seconds is four hours: comfortably longer than
# any refinery merge cycle or polecat verify-then-push, and short enough that a
# marker never survives a working day of toolchain drift.
gc_verified_tree_default_ttl_seconds=14400

# gc_verified_tree_id prints the identity of the content currently checked out,
# and fails when that content has no honest name. A dirty worktree is exactly
# that case: what a suite would run against is not what HEAD records.
gc_verified_tree_id() {
  local _gvt_dirty _gvt_tree

  _gvt_dirty="$(git status --porcelain 2>/dev/null)" || return 1
  if [ -n "$_gvt_dirty" ]; then
    return 1
  fi
  _gvt_tree="$(git rev-parse --verify --quiet 'HEAD^{tree}' 2>/dev/null)" || return 1
  [ -n "$_gvt_tree" ] || return 1

  printf '%s\n' "$_gvt_tree"
}

# gc_verified_tree_dir prints the directory markers live in. Anchored on the
# common git dir so linked worktrees of one clone share verdicts — which is
# sound precisely because the key is a content hash, so a shared marker only
# ever matches a worktree holding identical content.
gc_verified_tree_dir() {
  local _gvt_common

  # --git-common-dir (Git 2.5+) may print a path relative to $PWD, so
  # absolutize it here rather than with --path-format=absolute (Git 2.31+):
  # git rev-parse echoes an unrecognized option and still exits 0, which would
  # smuggle garbage past this check on older git.
  _gvt_common="$(git rev-parse --git-common-dir 2>/dev/null)" || return 1
  [ -n "$_gvt_common" ] || return 1
  _gvt_common="$(cd "$_gvt_common" 2>/dev/null && pwd)" || return 1

  printf '%s/verified-trees\n' "$_gvt_common"
}

# gc_verified_tree_marker prints the marker path for one suite over one tree.
# The mode is a path component, so it is reduced to safe characters even though
# every caller passes one of the runner's fixed suite names.
gc_verified_tree_marker() {
  local _gvt_mode="${1-}" _gvt_tree="${2-}" _gvt_dir _gvt_safe

  [ -n "$_gvt_mode" ] || return 1
  [ -n "$_gvt_tree" ] || return 1
  _gvt_dir="$(gc_verified_tree_dir)" || return 1
  _gvt_safe="$(printf '%s' "$_gvt_mode" | tr -c 'A-Za-z0-9._-' '_')"

  printf '%s/%s.%s\n' "$_gvt_dir" "$_gvt_safe" "$_gvt_tree"
}

# gc_verified_tree_ttl_seconds prints the configured lifetime of a verdict.
# A malformed override falls back to the default rather than failing a suite
# over an environment typo; 0 is a legitimate value meaning "never reuse".
gc_verified_tree_ttl_seconds() {
  local _gvt_ttl="${PUSH_GATE_VERIFIED_TTL_SECONDS-}"

  case "$_gvt_ttl" in
    '' | *[!0-9]*) _gvt_ttl="$gc_verified_tree_default_ttl_seconds" ;;
  esac

  printf '%s\n' "$_gvt_ttl"
}

# gc_verified_tree_mtime prints a file's modification time as a Unix timestamp.
# GNU and BSD stat disagree on the flag; this repo's suites run on both.
gc_verified_tree_mtime() {
  local _gvt_path="${1-}" _gvt_mtime

  [ -n "$_gvt_path" ] || return 1
  _gvt_mtime="$(stat -c %Y "$_gvt_path" 2>/dev/null)" || _gvt_mtime=""
  if [ -z "$_gvt_mtime" ]; then
    _gvt_mtime="$(stat -f %m "$_gvt_path" 2>/dev/null)" || _gvt_mtime=""
  fi
  case "$_gvt_mtime" in
    '' | *[!0-9]*) return 1 ;;
  esac

  printf '%s\n' "$_gvt_mtime"
}

# gc_verified_tree_prune deletes markers past their TTL. Every distinct tree
# ever verified leaves a file behind, so without this the cache grows for the
# life of the clone. Always succeeds: pruning is housekeeping, not a gate.
gc_verified_tree_prune() {
  local _gvt_dir _gvt_ttl _gvt_now _gvt_file _gvt_mtime

  _gvt_dir="$(gc_verified_tree_dir)" || return 0
  [ -d "$_gvt_dir" ] || return 0
  _gvt_ttl="$(gc_verified_tree_ttl_seconds)"
  _gvt_now="$(date +%s)"

  for _gvt_file in "$_gvt_dir"/*; do
    [ -f "$_gvt_file" ] || continue
    _gvt_mtime="$(gc_verified_tree_mtime "$_gvt_file")" || continue
    if [ "$(( _gvt_now - _gvt_mtime ))" -ge "$_gvt_ttl" ]; then
      rm -f "$_gvt_file"
    fi
  done

  return 0
}

# gc_verified_tree_record notes that mode passed over the tree checked out now.
#
# expected_tree, when given, is the tree the caller captured before starting the
# suite. If the content moved underneath the run, the verdict describes neither
# tree and is dropped.
#
# Always returns 0. Callers source this library under `set -e` around a suite
# that has already passed; a cache write must never be what turns that green
# run red.
#
# Usage: gc_verified_tree_record <mode> [expected_tree]
gc_verified_tree_record() {
  local _gvt_mode="${1-}" _gvt_expect="${2-}"
  local _gvt_tree _gvt_marker _gvt_dir _gvt_tmp

  [ -n "$_gvt_mode" ] || return 0
  _gvt_tree="$(gc_verified_tree_id)" || return 0
  if [ -n "$_gvt_expect" ] && [ "$_gvt_expect" != "$_gvt_tree" ]; then
    return 0
  fi
  _gvt_marker="$(gc_verified_tree_marker "$_gvt_mode" "$_gvt_tree")" || return 0
  _gvt_dir="$(gc_verified_tree_dir)" || return 0
  mkdir -p "$_gvt_dir" 2>/dev/null || return 0

  gc_verified_tree_prune

  # Written via temp + rename so a concurrent reader never sees a half-written
  # marker and read it as a verdict.
  _gvt_tmp="$_gvt_marker.tmp.$$"
  {
    printf 'mode=%s\n' "$_gvt_mode"
    printf 'tree=%s\n' "$_gvt_tree"
    printf 'recorded_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'recorded_by=%s\n' "${GC_SESSION_NAME:-${GC_AGENT:-${USER:-unknown}}}"
  } >"$_gvt_tmp" 2>/dev/null || {
    rm -f "$_gvt_tmp"
    return 0
  }
  mv -f "$_gvt_tmp" "$_gvt_marker" 2>/dev/null || rm -f "$_gvt_tmp"

  return 0
}

# gc_verified_tree_is_fresh succeeds when mode already has an unexpired verdict
# for the content checked out now — i.e. when running it again would re-derive
# an answer this clone already holds.
#
# Usage: gc_verified_tree_is_fresh <mode>
gc_verified_tree_is_fresh() {
  local _gvt_mode="${1-}"
  local _gvt_tree _gvt_marker _gvt_ttl _gvt_mtime _gvt_now _gvt_age

  [ -n "$_gvt_mode" ] || return 1
  [ -z "${PUSH_GATE_IGNORE_VERIFIED-}" ] || return 1
  _gvt_tree="$(gc_verified_tree_id)" || return 1
  _gvt_marker="$(gc_verified_tree_marker "$_gvt_mode" "$_gvt_tree")" || return 1
  [ -f "$_gvt_marker" ] || return 1
  _gvt_mtime="$(gc_verified_tree_mtime "$_gvt_marker")" || return 1

  _gvt_ttl="$(gc_verified_tree_ttl_seconds)"
  _gvt_now="$(date +%s)"
  _gvt_age=$(( _gvt_now - _gvt_mtime ))

  # A marker dated in the future came from a clock change, not from a run that
  # just finished; treat it as unusable rather than as indefinitely fresh.
  [ "$_gvt_age" -ge 0 ] || return 1

  [ "$_gvt_age" -lt "$_gvt_ttl" ]
}
