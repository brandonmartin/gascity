#!/bin/sh
# gc dolt logs — Tail the Dolt server log file.
#
# Usage: gc dolt logs [-n LINES] [-f] [--raw]
#
# Since ga-oigp lowered read_timeout_millis to 15s, the server reaps idle
# per-call connections on schedule and logs one ERROR line per reap. That is
# the fix working as designed, but it is emitted at error severity and at high
# volume — the observed server reached connection id ~1.1M in 15h — and it is
# the same line that used to be the first symptom of the alive-but-deaf wedge.
# dolt owns that severity, so the signal is restored here instead: by default
# the run is collapsed into one counted notice, and --raw shows the raw tail.
# The real wedge signatures (`use of closed network connection`, an empty
# `ss -lnt` for the port while the pid is alive) are untouched by the filter.
# See engdocs/contributors/dolt-recovery-single-flight.md.
#
# Environment: GC_CITY_PATH (set by gc pack command infrastructure)
set -e

lines=50
follow=false
raw=false

# Emit a partial count after this many consecutive suppressed lines so
# `-f` reports the flood as it happens instead of withholding the tally
# until a real line (or EOF) arrives.
NOISE_FLUSH_EVERY=500

PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

while [ $# -gt 0 ]; do
  case "$1" in
    -n|--lines) lines="$2"; shift 2 ;;
    -n*)        lines="${1#-n}"; shift ;;
    -f|--follow) follow=true; shift ;;
    --raw)      raw=true; shift ;;
    -h|--help)
      echo "Usage: gc dolt logs [-n LINES] [-f] [--raw]"
      echo ""
      echo "Tail the Dolt server log file."
      echo ""
      echo "By default, runs of by-design connection read-timeout reap lines"
      echo "are collapsed into a single counted notice. Those lines are"
      echo "expected operation since ga-oigp, not a wedge signal."
      echo ""
      echo "Flags:"
      echo "  -n, --lines N   Number of lines to show (default: 50)"
      echo "  -f, --follow    Follow the log in real time"
      echo "      --raw       Show every line, including the reap noise"
      exit 0
      ;;
    *) echo "gc dolt logs: unknown flag: $1" >&2; exit 1 ;;
  esac
done

log_file="$DOLT_LOG_FILE"
host="${GC_DOLT_HOST:-127.0.0.1}"

if [ ! -f "$log_file" ]; then
  if ! is_local_dolt_host "$host"; then
    # Configured external Dolt endpoint: the server log lives on the remote
    # host, not in this city's local pack state. A missing local log is an
    # expected limitation of pointing at an external endpoint, not a failure —
    # do not hard-fail the way a missing managed-server log would (su-deol8).
    echo "gc dolt logs: external Dolt endpoint $host:$GC_DOLT_PORT — server logs live on the remote host and are not available locally." >&2
    exit 0
  fi
  echo "gc dolt logs: log file not found: $log_file" >&2
  exit 1
fi

args="-n${lines}"
if [ "$follow" = true ]; then
  args="$args -f"
fi

if [ "$raw" = true ]; then
  exec tail $args "$log_file"
fi

# The filtered path is a pipeline, so a failing `tail` would otherwise be
# masked by awk's exit 0. Enable pipefail where the shell has it (bash, ksh,
# recent dash/busybox ash); shells without it keep the old masking behavior
# rather than failing to start.
if (set -o pipefail) 2>/dev/null; then
  set -o pipefail
fi

# Match on both substrings so the filter can only ever hide the read-timeout
# reap: a reap line naming a different cause (connection reset by peer) and a
# timeout from any other subsystem both fall through to the pass-through rule.
tail $args "$log_file" | awk -v flush_every="$NOISE_FLUSH_EVERY" '
function flush_noise() {
  if (suppressed > 0) {
    printf "gc dolt logs: suppressed %d by-design connection read-timeout reap line(s); expected since ga-oigp lowered read_timeout_millis, not a wedge signal (re-run with --raw to show them)\n", suppressed
    suppressed = 0
    fflush()
  }
}
index($0, "Error reading packet from client") > 0 && index($0, "i/o timeout") > 0 {
  suppressed++
  if (suppressed >= flush_every) { flush_noise() }
  next
}
{ flush_noise(); print; fflush() }
END { flush_noise() }
'
