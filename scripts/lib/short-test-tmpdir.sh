#!/usr/bin/env bash
# short-test-tmpdir.sh — keep go test's temp roots inside the sun_path budget.
#
# Unix socket paths are capped at 108 bytes on Linux and 104 on macOS.
# Go 1.26's testing.T.TempDir prefers GOTMPDIR when it is set, and otherwise
# uses os.TempDir (TMPDIR). A refinery cache directory such as
# /var/tmp/gc-refinery-bisect-<bead>.XXXXXX is long enough that socket tests
# fail on a clean tree. GOCACHE is not handled here: the build cache may
# stay in that long on-disk directory and must not move onto /tmp.
#
# gc_pin_short_test_tmpdir rewrites TMPDIR, GOTMPDIR, and a caller-scoped
# gotmpdir_val when any of them is longer than GC_TEST_TMPDIR_MAX (default
# 20). A path at or under the max, including the empty string, is preserved.
# The replacement is one directory, /var/tmp/g.XXXXXX, shared by every value
# this call rewrites. gc_cleanup_short_test_tmpdir removes that directory
# and nothing the caller already owned.

# GC_TEST_TMPDIR_MAX is the longest temp root passed through unchanged.
: "${GC_TEST_TMPDIR_MAX:=20}"

# gc_test_tmpdir_too_long returns 0 when value is set and over the budget.
gc_test_tmpdir_too_long() {
  local value="$1"
  [[ -n "$value" && ${#value} -gt ${GC_TEST_TMPDIR_MAX} ]]
}

# gc_make_short_test_tmpdir creates the replacement directory and prints it.
# The template is absolute so BSD and GNU mktemp both land on /var/tmp
# rather than $TMPDIR. /var/tmp/g.XXXXXX is 17 bytes.
gc_make_short_test_tmpdir() {
  mktemp -d /var/tmp/g.XXXXXX
}

# gc_pin_short_test_tmpdir applies the budget to the temp roots go test
# actually consults. gotmpdir_val is the snapshot the runners pass into
# env -i; it is updated in the caller when it is already set and too long.
# GC_TEST_SHORT_TMPDIR is intentionally not exported. Nested runners
# (test-integration-shard re-exec, test-go-test-shard) source this file and
# would otherwise delete the parent's directory on their own EXIT trap.
gc_pin_short_test_tmpdir() {
  local short="${GC_TEST_SHORT_TMPDIR:-}"
  if gc_test_tmpdir_too_long "${TMPDIR:-}"; then
    if [[ -z "$short" ]]; then
      short="$(gc_make_short_test_tmpdir)"
      GC_TEST_SHORT_TMPDIR="$short"
    fi
    export TMPDIR="$short"
  fi
  if gc_test_tmpdir_too_long "${GOTMPDIR:-}"; then
    if [[ -z "${GC_TEST_SHORT_TMPDIR:-}" ]]; then
      short="$(gc_make_short_test_tmpdir)"
      GC_TEST_SHORT_TMPDIR="$short"
    fi
    export GOTMPDIR="${GC_TEST_SHORT_TMPDIR}"
  fi
  if [[ -n "${gotmpdir_val:-}" ]] && gc_test_tmpdir_too_long "$gotmpdir_val"; then
    if [[ -z "${GC_TEST_SHORT_TMPDIR:-}" ]]; then
      short="$(gc_make_short_test_tmpdir)"
      GC_TEST_SHORT_TMPDIR="$short"
    fi
    gotmpdir_val="${GC_TEST_SHORT_TMPDIR}"
  fi
}

# gc_cleanup_short_test_tmpdir removes a directory this file created.
gc_cleanup_short_test_tmpdir() {
  if [[ -n "${GC_TEST_SHORT_TMPDIR:-}" ]]; then
    rm -rf "${GC_TEST_SHORT_TMPDIR}"
    unset GC_TEST_SHORT_TMPDIR
  fi
}
