# shellcheck shell=bash
#
# Shared classification of "this run was killed" versus "this run failed", for
# every push-gate layer that has to report a suite's outcome.
#
# A killed sweep is neither a pass nor a failure: no test ever reported a
# verdict, so the tree under test was never judged. Reported as a bare nonzero
# it reads as a regression, and the next agent retries straight into whatever
# killed it (ga-8qmy). scripts/go-test-observable has said "KILLED by signal
# <n>" in words since ga-spc; this library is that same rule, factored out so
# the parallel runner and .githooks/pre-push state it identically instead of
# surfacing a generic `make` error.
#
# Two encodings reach a caller, and both mean the same thing:
#
#   128+N   a shell's wait status for a child that died to signal N. The
#           conventional bound is `> 128`, matching go-test-observable and
#           TESTING.md's "Shared-Host Test Conventions".
#   125     GNU xargs' documented status for "the command was killed by a
#           signal". scripts/test-local-parallel fans out through `xargs -P`,
#           which collapses 128+N to 125 rather than passing it through, so a
#           signal-killed job arrives at the epilogue looking like a plain
#           nonzero unless this case is handled.
#
# Deliberately NOT treated as signal death: 75 (the push gate's own
# EX_TEMPFAIL for slot-wait exhaustion), 123/124 (xargs' "a command exited
# nonzero" relays) and 126/127 (exec failures). Those are real outcomes with
# their own meanings and must keep reading as themselves.

# gc_signal_status_xargs_killed is GNU xargs' exit status for a command that
# was killed by a signal.
gc_signal_status_xargs_killed=125

# gc_status_is_numeric succeeds when status is a bare non-negative integer.
# Guarding first keeps `[` from erroring on a malformed status and keeps a
# genuine failure from being excused as a kill.
gc_status_is_numeric() {
  case "${1-}" in
    '' | *[!0-9]*) return 1 ;;
    *) return 0 ;;
  esac
}

# gc_status_is_signal_death succeeds when status means the run was terminated
# by a signal rather than finishing with a verdict.
gc_status_is_signal_death() {
  local status="${1-}"
  gc_status_is_numeric "$status" || return 1
  if [ "$status" -gt 128 ]; then
    return 0
  fi
  [ "$status" -eq "$gc_signal_status_xargs_killed" ]
}

# gc_signal_status_describe prints a human phrase naming how the run was
# killed, and fails without output when status is an ordinary result. Callers
# embed the phrase in their own message so each layer keeps its own voice.
gc_signal_status_describe() {
  local status="${1-}"
  gc_status_is_numeric "$status" || return 1
  if [ "$status" -gt 128 ]; then
    printf 'killed by signal %s (exit %s)' "$((status - 128))" "$status"
    return 0
  fi
  if [ "$status" -eq "$gc_signal_status_xargs_killed" ]; then
    printf 'killed by a signal (xargs exit %s)' "$status"
    return 0
  fi
  return 1
}

# gc_report_job_outcome prints one line describing how a single fan-out job
# ended. A killed job is called out by name: it produced no verdict, so it is
# evidence about the host, not about the diff. The wording for an ordinary
# failure is unchanged.
gc_report_job_outcome() {
  local label="$1" status="$2" log="$3" phrase
  if phrase="$(gc_signal_status_describe "$status")"; then
    printf '[%s] KILLED — %s; the job produced no verdict; log: %s\n' "$label" "$phrase" "$log"
  else
    printf '[%s] failed with exit %s; log: %s\n' "$label" "$status" "$log"
  fi
}

# gc_report_suite_outcome prints the closing verdict for a whole fan-out --
# the line an agent reads to decide what just happened. Under ga-8qmy a sweep
# that was killed with zero FAIL lines in any log still closed with "One or
# more fast jobs failed", so three consecutive reaps read as flaky tests.
gc_report_suite_outcome() {
  local mode="$1" status="$2" log_dir="$3" phrase
  if phrase="$(gc_signal_status_describe "$status")"; then
    printf '%s suite ABORTED — %s.\n' "$mode" "$phrase"
    printf 'This is NOT a test failure: the sweep was terminated before its jobs\n'
    printf 'reported verdicts, so an empty FAIL list proves nothing. Do not retry\n'
    printf 'blindly — find what killed it first (TESTING.md, "Shared-Host Test\n'
    printf 'Conventions"). Partial logs are in %s\n' "$log_dir"
  else
    printf 'One or more %s jobs failed; logs are in %s\n' "$mode" "$log_dir"
  fi
}
