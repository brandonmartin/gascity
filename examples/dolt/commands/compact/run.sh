#!/bin/sh
# gc dolt compact — flatten Dolt commit history on managed databases.
#
# Why this exists: every bead mutation creates a Dolt commit. Over time
# this builds an enormous commit graph (thousands of commits/day on busy
# cities). The commit graph IS the storage cost — DOLT_GC alone cannot
# reclaim space when all commits are live history. Flattening squashes
# the graph into a single commit and lets the next DOLT_GC reclaim
# orphaned chunks.
#
# This command replaces the formula-based mol-dog-compactor that was
# routed to the dog pool. Per the formula's own ZFC-exemption notice,
# compaction requires SQL access (database/sql) that agents don't have.
# Running as an exec order (like dolt-gc-nudge) gives us direct SQL
# access via the dolt CLI.
#
# Algorithm (flatten mode):
#   1. Pre-flight: record row counts for all user tables.
#   2. Soft-reset to the root commit; all data stays staged.
#   3. Commit everything as a single "compaction: flatten history" commit.
#   4. Verify post-flatten row counts match pre-flight.
#   5. Run CALL DOLT_GC() to reclaim chunks orphaned by the flatten.
#
# Concurrent writes are safe — merge base shifts but data is preserved.
# Surgical mode (preserve recent N commits via interactive rebase) is
# intentionally not implemented; flatten is sufficient for bloat recovery
# and avoids the rebase-vs-concurrent-write hazards.
#
# Runs from the dolt pack's mol-dog-compactor order.
#
# Environment:
#   GC_CITY_PATH                          (required) — city root
#   GC_DOLT_PORT                          (required) — managed dolt port
#   GC_DOLT_HOST                          (default: 127.0.0.1)
#   GC_DOLT_USER                          (default: root)
#   GC_DOLT_PASSWORD                      (optional)
#   GC_DOLT_COMPACT_THRESHOLD_COMMITS
#     (default: 500) — skip databases with fewer commits than this.
#   GC_DOLT_COMPACT_CALL_TIMEOUT_SECS
#     (default: 1800) — wall-clock bound for each SQL CALL.
#   GC_DOLT_COMPACT_DRY_RUN              (optional) — when set, prints
#                                         what would happen but does not
#                                         execute any DOLT_RESET / COMMIT.
#   GC_DOLT_COMPACT_ONLY_DBS              (optional) — comma-separated list of
#                                         database names to compact. When set,
#                                         all other databases are skipped.
set -eu

: "${GC_CITY_PATH:?GC_CITY_PATH must be set}"
: "${GC_DOLT_PORT:=}"
gc_dolt_port_input="$GC_DOLT_PORT"
gc_dolt_host_input="${GC_DOLT_HOST:-}"

PACK_DIR="${GC_PACK_DIR:-$(unset CDPATH; cd -- "$(dirname "$0")/.." && pwd)}"
# shellcheck disable=SC1091
. "$PACK_DIR/assets/scripts/runtime.sh"

case "${GC_DOLT_MANAGED_LOCAL:-}" in
  0|false|FALSE|no|NO)
    printf 'compact: managed local Dolt runtime is not applicable for this order — skip\n'
    exit 0
    ;;
esac

if [ "${GC_DOLT_MANAGED_LOCAL:-}" = "1" ]; then
  managed_port=$(managed_runtime_port "$DOLT_STATE_FILE" "$DOLT_DATA_DIR" || true)
  if [ -n "$managed_port" ]; then
    if [ -n "$gc_dolt_port_input" ] && [ "$gc_dolt_port_input" != "$managed_port" ]; then
      printf 'compact: GC_DOLT_PORT=%s does not match managed runtime port=%s for data_dir=%s — skip\n' \
        "$gc_dolt_port_input" "$managed_port" "$DOLT_DATA_DIR"
      exit 0
    fi
    GC_DOLT_PORT="$managed_port"
  elif [ -z "$gc_dolt_port_input" ]; then
    printf 'compact: managed local Dolt runtime is not active for data_dir=%s — skip\n' \
      "$DOLT_DATA_DIR"
    exit 0
  else
    GC_DOLT_PORT="$gc_dolt_port_input"
  fi
elif [ -n "$gc_dolt_port_input" ]; then
  case "$gc_dolt_host_input" in
    ''|127.0.0.1|localhost|0.0.0.0|::1|::|'[::1]'|'[::]')
      ;;
    *)
      printf 'compact: GC_DOLT_HOST=%s is not a local managed Dolt host — skip\n' \
        "$gc_dolt_host_input"
      exit 0
      ;;
  esac
  managed_port=$(managed_runtime_port "$DOLT_STATE_FILE" "$DOLT_DATA_DIR" || true)
  if [ -z "$managed_port" ] || [ "$gc_dolt_port_input" != "$managed_port" ]; then
    printf 'compact: GC_DOLT_PORT=%s does not match managed runtime port=%s for data_dir=%s — skip\n' \
      "$gc_dolt_port_input" "${managed_port:-<inactive>}" "$DOLT_DATA_DIR"
    exit 0
  fi
  GC_DOLT_PORT="$managed_port"
elif [ -z "$gc_dolt_port_input" ]; then
  managed_port=$(managed_runtime_port "$DOLT_STATE_FILE" "$DOLT_DATA_DIR" || true)
  if [ -z "$managed_port" ]; then
    printf 'compact: managed local Dolt runtime is not active for data_dir=%s — skip\n' \
      "$DOLT_DATA_DIR"
    exit 0
  fi
  GC_DOLT_PORT="$managed_port"
fi

: "${GC_DOLT_PORT:?GC_DOLT_PORT must be set}"
: "${GC_DOLT_USER:=root}"

host="${GC_DOLT_HOST:-127.0.0.1}"
threshold_commits="${GC_DOLT_COMPACT_THRESHOLD_COMMITS:-500}"
call_timeout="${GC_DOLT_COMPACT_CALL_TIMEOUT_SECS:-1800}"
dry_run="${GC_DOLT_COMPACT_DRY_RUN:-}"
only_dbs="${GC_DOLT_COMPACT_ONLY_DBS:-}"

case "$threshold_commits" in
  ''|*[!0-9]*)
    printf 'compact: invalid GC_DOLT_COMPACT_THRESHOLD_COMMITS=%s (must be a non-negative integer)\n' \
      "$threshold_commits" >&2
    exit 2
    ;;
esac

case "$call_timeout" in
  ''|*[!0-9]*|0)
    printf 'compact: invalid GC_DOLT_COMPACT_CALL_TIMEOUT_SECS=%s (must be a positive integer)\n' \
      "$call_timeout" >&2
    exit 2
    ;;
esac

# Cross-city flock keyed on host:port so concurrent compactions on the
# same Dolt server don't interleave. Compaction holds open transactions
# and a second compactor running concurrently would race on the
# graph-rewrite step.
lock_host=$(printf '%s' "$host" | tr '[:upper:]' '[:lower:]' | sed 's/^\[\(.*\)\]$/\1/')
case "$lock_host" in
  ''|127.0.0.1|localhost|0.0.0.0|::1|::)
    lock_host="127.0.0.1"
    ;;
esac
lock_key=$(printf '%s-%s' "$lock_host" "$GC_DOLT_PORT" | tr -c 'A-Za-z0-9_.-' '-')
lock_root="/tmp/gc-dolt-compact"
mkdir -p "$lock_root"
chmod 700 "$lock_root" 2>/dev/null || true
lock_path="$lock_root/${lock_key}.lock"

# Same DB-discovery pattern as gc-nudge: rig metadata.json files first
# (authoritative), with a filesystem-scan fallback when gc itself is
# unavailable.
metadata_files() {
  printf '%s\n' "$GC_CITY_PATH/.beads/metadata.json"
  if command -v gc >/dev/null 2>&1; then
    if rig_json=$(run_bounded 5 gc rig list --json 2>/dev/null); then
      rig_paths=$(printf '%s\n' "$rig_json" \
        | if command -v jq >/dev/null 2>&1; then
            jq -r '.rigs[].path' 2>/dev/null
          else
            grep '"path"' | sed 's/.*"path": *"//;s/".*//'
          fi) || true
      if [ -n "$rig_paths" ]; then
        printf '%s\n' "$rig_paths" | while IFS= read -r p; do
          [ -n "$p" ] && printf '%s\n' "$p/.beads/metadata.json"
        done
        return
      fi
    else
      rig_status=$?
      if [ "$rig_status" -eq 124 ]; then
        printf 'compact: gc rig list timed out after 5s; falling back to local filesystem metadata scan\n' >&2
      else
        printf 'compact: gc rig list failed rc=%s; falling back to local filesystem metadata scan\n' "$rig_status" >&2
      fi
    fi
  fi
  find "$GC_CITY_PATH" \
    \( -path "$GC_CITY_PATH/.gc" -o -path "$GC_CITY_PATH/.git" \) -prune -o \
    -path '*/.beads/metadata.json' -print 2>/dev/null || true
}

metadata_db() {
  meta="$1"
  db=""
  if [ ! -f "$meta" ]; then
    printf '%s\n' "beads"
    return 0
  fi
  if command -v jq >/dev/null 2>&1; then
    db=$(jq -r '.dolt_database // empty' "$meta" 2>/dev/null || true)
  else
    db=$(grep -o '"dolt_database"[[:space:]]*:[[:space:]]*"[^"]*"' "$meta" 2>/dev/null \
      | sed 's/.*: *"//;s/"$//' || true)
  fi
  if [ -z "$db" ]; then
    db="beads"
  fi
  printf '%s\n' "$db"
}

valid_database_name() {
  db="$1"
  case "$db" in
    [A-Za-z0-9_]*)
      case "$db" in
        *[!A-Za-z0-9_-]*) return 1 ;;
        *) return 0 ;;
      esac
      ;;
    *) return 1 ;;
  esac
}

is_system_database() {
  name=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')
  case "$name" in
    information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe) return 0 ;;
    *) return 1 ;;
  esac
}

emit_database_name() {
  db="$1"
  if ! valid_database_name "$db"; then
    printf 'compact: db=%s invalid database name — skip\n' "$db" >&2
    return 0
  fi
  if is_system_database "$db"; then
    printf 'compact: db=%s system database — skip\n' "$db" >&2
    return 0
  fi
  printf '%s\n' "$db"
}

discover_database_names() {
  while IFS= read -r meta; do
    [ -n "$meta" ] || continue
    db=$(metadata_db "$meta")
    emit_database_name "$db"
  done < "$_meta_tmp"

  if [ -d "$DOLT_DATA_DIR" ]; then
    for d in "$DOLT_DATA_DIR"/*/; do
      [ -d "$d/.dolt" ] || continue
      db=${d%/}
      db=${db##*/}
      is_system_database "$db" && continue
      emit_database_name "$db"
    done
  fi
}

# dolt_query — wrapper that runs a single SQL statement against the
# managed server with the configured port/host/user. Honors the
# per-call timeout. Output is the raw -r result-format-tsv body.
dolt_query() {
  db="$1"
  query="$2"
  export DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}"
  run_bounded "$call_timeout" \
    dolt --host "$host" --port "$GC_DOLT_PORT" \
    --user "$GC_DOLT_USER" --no-tls \
    --use-db "$db" \
    sql -r tabular -q "$query"
}

# commit_count — count of commits reachable from main. Bounded scan
# (LIMIT 200000) so a runaway DB doesn't tie up the connection.
commit_count() {
  db="$1"
  dolt_query "$db" "SELECT COUNT(*) FROM (SELECT 1 FROM dolt_log LIMIT 200000) AS t" 2>/dev/null \
    | awk 'NR==4 {gsub(/[| ]/, ""); print; exit}'
}

# root_commit — earliest commit hash on the main branch.
root_commit() {
  db="$1"
  dolt_query "$db" "SELECT commit_hash FROM dolt_log ORDER BY date ASC LIMIT 1" 2>/dev/null \
    | awk 'NR==4 {gsub(/[| ]/, ""); print; exit}'
}

# user_tables — emit one user-table name per line (excludes dolt_*
# system tables and information_schema views).
user_tables() {
  db="$1"
  dolt_query "$db" \
    "SELECT table_name FROM information_schema.tables WHERE table_schema = '$db' AND table_name NOT LIKE 'dolt\\_%' ESCAPE '\\\\' ORDER BY table_name" \
    2>/dev/null \
    | awk 'NR>=4 && /^\|/ {gsub(/^\| | \|$/, ""); gsub(/ /, ""); if ($0 != "") print}'
}

# row_count — COUNT(*) for one table. Returns "" on error.
row_count() {
  db="$1"
  table="$2"
  dolt_query "$db" "SELECT COUNT(*) FROM \`$table\`" 2>/dev/null \
    | awk 'NR==4 {gsub(/[| ]/, ""); print; exit}'
}

# preflight_counts — write "<table> <count>" lines for all user tables.
preflight_counts() {
  db="$1"
  out="$2"
  : > "$out"
  user_tables "$db" | while IFS= read -r t; do
    [ -n "$t" ] || continue
    cnt=$(row_count "$db" "$t")
    case "$cnt" in
      ''|*[!0-9]*)
        printf 'compact: db=%s pre-flight row count failed for table=%s\n' "$db" "$t" >&2
        return 1
        ;;
    esac
    printf '%s %s\n' "$t" "$cnt" >> "$out"
  done
}

# verify_counts — re-count and compare against the pre-flight file.
# Returns nonzero on any mismatch.
verify_counts() {
  db="$1"
  preflight="$2"
  fail=0
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    t=${line%% *}
    expected=${line##* }
    actual=$(row_count "$db" "$t")
    case "$actual" in
      ''|*[!0-9]*)
        printf 'compact: db=%s post-flatten row count failed for table=%s\n' "$db" "$t" >&2
        fail=1
        continue
        ;;
    esac
    if [ "$actual" != "$expected" ]; then
      printf 'compact: db=%s INTEGRITY FAILURE table=%s expected=%s actual=%s\n' \
        "$db" "$t" "$expected" "$actual" >&2
      fail=1
    fi
  done < "$preflight"
  return "$fail"
}

flatten_database() {
  db="$1"

  if [ -n "$only_dbs" ]; then
    case ",$only_dbs," in
      *,"$db",*) ;;
      *)
        printf 'compact: db=%s not in GC_DOLT_COMPACT_ONLY_DBS — skip\n' "$db"
        return 0
        ;;
    esac
  fi

  count=$(commit_count "$db")
  case "$count" in
    ''|*[!0-9]*)
      printf 'compact: db=%s commit count probe failed — skip\n' "$db" >&2
      return 0
      ;;
  esac

  if [ "$count" -lt "$threshold_commits" ]; then
    printf 'compact: db=%s commits=%s below_threshold=%s — skip\n' \
      "$db" "$count" "$threshold_commits"
    return 0
  fi

  root=$(root_commit "$db")
  if [ -z "$root" ]; then
    printf 'compact: db=%s root commit probe failed — skip\n' "$db" >&2
    return 1
  fi

  if [ -n "$dry_run" ]; then
    printf 'compact: db=%s commits=%s root=%s — dry-run (would flatten)\n' \
      "$db" "$count" "$root"
    return 0
  fi

  preflight_tmp=$(mktemp)
  if ! preflight_counts "$db" "$preflight_tmp"; then
    rm -f "$preflight_tmp"
    return 1
  fi
  table_count=$(wc -l < "$preflight_tmp")
  printf 'compact: db=%s commits=%s root=%s tables=%s — flattening...\n' \
    "$db" "$count" "$root" "$table_count"

  start=$(date +%s)

  # Soft-reset to root + commit-everything is the flatten transaction.
  # Both run in a single dolt sql invocation so the session keeps the
  # USE selection across the two CALLs.
  reset_rc=0
  dolt_query "$db" "
    CALL DOLT_RESET('--soft', '$root');
    CALL DOLT_COMMIT('-Am', 'compaction: flatten history');
  " >/dev/null 2>&1 || reset_rc=$?

  if [ "$reset_rc" -ne 0 ]; then
    printf 'compact: db=%s flatten failed rc=%s — leaving database in pre-flatten state\n' \
      "$db" "$reset_rc" >&2
    rm -f "$preflight_tmp"
    return 1
  fi

  if ! verify_counts "$db" "$preflight_tmp"; then
    printf 'compact: db=%s post-flatten INTEGRITY check failed — escalate (data preserved, but row counts diverge)\n' \
      "$db" >&2
    rm -f "$preflight_tmp"
    return 1
  fi
  rm -f "$preflight_tmp"

  after_count=$(commit_count "$db")

  # CALL DOLT_GC() alone only reclaims working-set chunks — the bulk of
  # the orphaned history lives in noms/oldgen/ archives that require
  # --full to rewrite. Since flatten always orphans the entire prior
  # commit graph, --full is always appropriate here (unlike the hourly
  # gc-nudge which uses the cheaper default GC).
  printf 'compact: db=%s — running DOLT_GC --full...\n' "$db"
  gc_rc=0
  dolt_query "$db" "CALL DOLT_GC('--full')" >/dev/null 2>&1 || gc_rc=$?

  elapsed=$(( $(date +%s) - start ))
  if [ "$gc_rc" -ne 0 ]; then
    printf 'compact: db=%s flatten ok commits=%s->%s but DOLT_GC failed rc=%s duration=%ss\n' \
      "$db" "$count" "${after_count:-?}" "$gc_rc" "$elapsed" >&2
    return 1
  fi

  printf 'compact: db=%s commits=%s->%s duration=%ss — ok\n' \
    "$db" "$count" "${after_count:-?}" "$elapsed"
  return 0
}

cleanup() {
  if [ -n "${_meta_tmp:-}" ]; then
    rm -f "$_meta_tmp"
  fi
  if [ -n "${_db_tmp:-}" ]; then
    rm -f "$_db_tmp"
  fi
  if [ -n "${_unique_db_tmp:-}" ]; then
    rm -f "$_unique_db_tmp"
  fi
}

main() {
  trap cleanup EXIT

  # Non-blocking flock — if another compactor is running for the same
  # host:port, skip rather than queue up.
  if command -v flock >/dev/null 2>&1; then
    old_umask=$(umask)
    umask 077
    : >> "$lock_path" 2>/dev/null || {
      umask "$old_umask"
      printf 'compact: unable to create lock file %s\n' "$lock_path" >&2
      exit 1
    }
    exec 9<>"$lock_path"
    umask "$old_umask"
    chmod 600 "$lock_path" 2>/dev/null || true
    if ! flock -n 9; then
      printf 'compact: another compaction already running for %s:%s — skipping\n' \
        "$host" "$GC_DOLT_PORT"
      exit 0
    fi
  fi

  _meta_tmp=$(mktemp)
  metadata_files > "$_meta_tmp"

  _db_tmp=$(mktemp)
  _unique_db_tmp=$(mktemp)
  discover_database_names > "$_db_tmp"

  seen_dbs=""
  while IFS= read -r db; do
    [ -n "$db" ] || continue
    case " $seen_dbs " in
      *" $db "*) continue ;;
    esac
    seen_dbs="$seen_dbs $db"
    printf '%s\n' "$db" >> "$_unique_db_tmp"
  done < "$_db_tmp"

  failed_count=0
  while IFS= read -r db; do
    [ -n "$db" ] || continue
    if ! flatten_database "$db"; then
      failed_count=$((failed_count + 1))
    fi
  done < "$_unique_db_tmp"

  if [ "$failed_count" -gt 0 ]; then
    printf 'compact: %s database(s) failed compaction\n' "$failed_count" >&2
    exit 1
  fi
  exit 0
}

main "$@"
