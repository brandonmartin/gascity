#!/usr/bin/env bash
#
# test-census-owner-liveness.sh — unit tests for the census-owner-liveness
# order wrapper (scripts/check-census-owner-liveness.sh), covering the
# duplicate-filing regression in ga-xw25.
#
# The wrapper's job is detection-only: file exactly one alert bead per
# distinct dangling owner_bead, and never re-file one that is already
# tracked. The regression was in the dedup query, which was narrowed two
# independent ways -- by status (`--status open`) and by label -- so a
# condition that was already tracked, and being worked, read as untracked
# on the next cron tick and got re-filed.
#
# These tests pin the dedup contract against a fake `bd` that models bd's
# real list-filter semantics (--all / --status / --label / --metadata-field),
# so narrowing the query again fails the suite rather than silently
# reintroducing the spam.
#
# Hermetic: fake `gc` and fake `bd` on PATH, temp dirs only. No network, no
# real bead store, no models.

set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$TEST_DIR/check-census-owner-liveness.sh"

pass=0; fail=0
record_pass() { echo "  ok   $1"; pass=$((pass + 1)); }
record_fail() { echo "  FAIL $1 — $2"; fail=$((fail + 1)); }

# ---------------------------------------------------------------------------
# Fakes. `gc doctor --json` and `bd` are both driven from <env>/state so each
# test writes exactly the world it needs.
#
#   <env>/state/doctor-json    -- body of `gc doctor --json`
#   <env>/state/doctor-exit    -- exit code for `gc doctor` (default 0); the
#                                 wrapper must tolerate nonzero, since doctor
#                                 exits nonzero on unrelated BLOCKING checks
#   <env>/state/beads.json     -- the bead store the fake `bd list` filters
#   <env>/state/created.jsonl  -- one line per `bd create` (the assertion
#                                 surface: what the wrapper actually filed)
# ---------------------------------------------------------------------------

# new_env: build a tmp dir holding the fakes plus their state. Prints its path.
new_env() {
    local d
    d="$(mktemp -d "${TMPDIR:-/tmp}/gc-col-test.XXXXXX")"
    mkdir -p "$d/state"
    printf '[]' > "$d/state/beads.json"
    : > "$d/state/created.jsonl"

    cat > "$d/gc" <<'FAKE_GC'
#!/usr/bin/env bash
set -uo pipefail
state="$(dirname "$0")/state"
if [ "${1:-}" = "doctor" ]; then
    [ -f "$state/doctor-json" ] && cat "$state/doctor-json"
    if [ -f "$state/doctor-exit" ]; then
        exit "$(cat "$state/doctor-exit")"
    fi
    exit 0
fi
exit 1
FAKE_GC
    chmod +x "$d/gc"

    # Fake `bd` modelling the real list-filter semantics the wrapper depends
    # on. Fidelity here is the point: if the wrapper narrows its query again
    # (by status or by label), this fake filters the suppressing bead out
    # exactly as real bd would, and the regression tests fail.
    cat > "$d/bd" <<'FAKE_BD'
#!/usr/bin/env bash
set -uo pipefail
state="$(dirname "$0")/state"

json_array_from_lines() { jq -R -s 'split("\n") | map(select(length > 0))'; }

cmd="${1:-}"; shift || true
case "$cmd" in
  list)
    all=0; statuses=""; labels=(); mfields=()
    while [ $# -gt 0 ]; do
      case "$1" in
        --all) all=1; shift ;;
        -s|--status) statuses="${2:-}"; shift 2 ;;
        --status=*) statuses="${1#--status=}"; shift ;;
        -l|--label) labels+=("${2:-}"); shift 2 ;;
        --label=*) labels+=("${1#--label=}"); shift ;;
        --metadata-field) mfields+=("${2:-}"); shift 2 ;;
        --metadata-field=*) mfields+=("${1#--metadata-field=}"); shift ;;
        -n|--limit) shift 2 ;;
        --limit=*) shift ;;
        *) shift ;;
      esac
    done

    labels_json="$(printf '%s\n' ${labels[@]+"${labels[@]}"} | json_array_from_lines)"
    mfields_json="$(printf '%s\n' ${mfields[@]+"${mfields[@]}"} | json_array_from_lines)"

    jq -n \
      --slurpfile store "$state/beads.json" \
      --argjson all "$all" \
      --arg statuses "$statuses" \
      --argjson labels "$labels_json" \
      --argjson mfields "$mfields_json" '
      $store[0]
      | map(. as $b | select(
          if $all == 1 then true
          elif ($statuses | length) > 0 then (($statuses | split(",")) | index($b.status) != null)
          else $b.status != "closed"
          end))
      | map(. as $b | select(
          $labels | map(. as $l | ((($b.labels) // []) | index($l) != null)) | all))
      | map(. as $b | select(
          $mfields | map(
            split("=") as $kv
            | (((($b.metadata) // {})[$kv[0]]) // null) == $kv[1]) | all))
    '
    exit 0
    ;;
  create)
    title=""; metadata="{}"; description=""; label=""; btype=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --title) title="${2:-}"; shift 2 ;;
        --metadata) metadata="${2:-}"; shift 2 ;;
        --description) description="${2:-}"; shift 2 ;;
        --label) label="${2:-}"; shift 2 ;;
        --type) btype="${2:-}"; shift 2 ;;
        *) shift ;;
      esac
    done

    n=$(( $(cat "$state/create-count" 2>/dev/null || echo 0) + 1 ))
    printf '%s' "$n" > "$state/create-count"
    new_id="ga-fake$n"

    bead="$(jq -n --arg id "$new_id" --arg title "$title" --arg label "$label" \
        --arg desc "$description" --arg btype "$btype" --argjson md "$metadata" \
        '{id:$id,title:$title,status:"open",issue_type:$btype,labels:[$label],metadata:$md,description:$desc}')"

    printf '%s\n' "$(printf '%s' "$bead" | jq -c .)" >> "$state/created.jsonl"

    # Keep the store honest so a later iteration in the same run dedups
    # against what this run already filed.
    jq --argjson bead "$bead" '. + [$bead]' "$state/beads.json" > "$state/beads.json.tmp" \
        && mv -f "$state/beads.json.tmp" "$state/beads.json"

    printf '%s\n' "$new_id"
    exit 0
    ;;
  *)
    exit 1
    ;;
esac
FAKE_BD
    chmod +x "$d/bd"

    printf '%s' "$d"
}

# set_doctor_warning <env> <detail-line>...: doctor reports the
# census-owner-liveness check as a warning carrying the given detail lines.
set_doctor_warning() {
    local env="$1"; shift
    printf '%s\n' "$@" | jq -R -s 'split("\n") | map(select(length > 0))' \
        | jq '{results: [{name: "census-owner-liveness", status: "warning", details: .}]}' \
        > "$env/state/doctor-json"
}

# add_bead <env> <id> <status> <owner_bead> [labels-json]: seed one bead
# carrying census.owner_bead metadata into the fake store.
add_bead() {
    local env="$1" id="$2" status="$3" owner_bead="$4" labels="${5:-[]}"
    jq --arg id "$id" --arg status "$status" --arg ob "$owner_bead" --argjson labels "$labels" \
        '. + [{id:$id, status:$status, labels:$labels, metadata:{"census.owner_bead":$ob}}]' \
        "$env/state/beads.json" > "$env/state/beads.json.tmp"
    mv -f "$env/state/beads.json.tmp" "$env/state/beads.json"
}

# run_check <env>: run the wrapper with the fakes on PATH. Prints combined
# output; returns the wrapper's exit code.
run_check() {
    local env="$1"
    PATH="$env:$PATH" "$SCRIPT" 2>&1
}

# created_count <env>: how many beads the wrapper filed.
created_count() { awk 'NF' "$1/state/created.jsonl" 2>/dev/null | wc -l | tr -d ' '; }

# created_owner_beads <env>: the census.owner_bead values it filed for.
created_owner_beads() {
    jq -r '.metadata["census.owner_bead"]' < "$1/state/created.jsonl" 2>/dev/null | sort
}

PATROL_LABEL="source:census-owner-liveness-patrol"
DANGLING="row test/test-resources.toml:12: dangling owner_bead=ga-80po0c.2 (resource gpu-a)"
DANGLING_OTHER="row test/test-resources.toml:19: dangling owner_bead=ga-c1slhq (resource gpu-b)"

# ---------------------------------------------------------------------------
# Baseline: an untracked dangling owner_bead still gets exactly one alert.
# ---------------------------------------------------------------------------

test_files_alert_when_untracked() {
    local env out rc
    env="$(new_env)"
    set_doctor_warning "$env" "$DANGLING"

    out="$(run_check "$env")"; rc=$?

    if [ "$rc" -ne 0 ]; then
        record_fail "files_alert_when_untracked" "exit $rc, want 0: $out"
        return
    fi
    if [ "$(created_count "$env")" != "1" ]; then
        record_fail "files_alert_when_untracked" "created $(created_count "$env") beads, want 1: $out"
        return
    fi
    if [ "$(created_owner_beads "$env")" != "ga-80po0c.2" ]; then
        record_fail "files_alert_when_untracked" "filed for $(created_owner_beads "$env"), want ga-80po0c.2"
        return
    fi
    record_pass "files_alert_when_untracked"
}

# ---------------------------------------------------------------------------
# The dedup contract. Each of these is a status or shape the pre-ga-xw25
# query missed, and each one re-filed a duplicate on every cron tick.
# ---------------------------------------------------------------------------

# assert_suppressed_by <name> <status> <labels-json>: seeding one bead in the
# given status/labels must suppress the alert entirely.
assert_suppressed_by() {
    local name="$1" status="$2" labels="$3"
    local env out rc
    env="$(new_env)"
    set_doctor_warning "$env" "$DANGLING"
    add_bead "$env" "ga-existing" "$status" "ga-80po0c.2" "$labels"

    out="$(run_check "$env")"; rc=$?

    if [ "$rc" -ne 0 ]; then
        record_fail "$name" "exit $rc, want 0: $out"
        return
    fi
    if [ "$(created_count "$env")" != "0" ]; then
        record_fail "$name" "re-filed $(created_count "$env") duplicate(s) despite ga-existing ($status): $out"
        return
    fi
    # Suppression must stay visible — a silent skip is indistinguishable from
    # a broken detector.
    if ! printf '%s' "$out" | grep -q "ga-existing"; then
        record_fail "$name" "skip message does not name the suppressing bead: $out"
        return
    fi
    record_pass "$name"
}

test_suppressed_by_open_alert() {
    assert_suppressed_by "suppressed_by_open_alert" "open" "[\"$PATROL_LABEL\"]"
}

# Regression (ga-xw25): a polecat claiming the bead flips it to in_progress,
# which the `--status open` query could not see.
test_suppressed_by_in_progress_alert() {
    assert_suppressed_by "suppressed_by_in_progress_alert" "in_progress" "[\"$PATROL_LABEL\"]"
}

# Regression (ga-xw25): the mayor closes symptom beads when consolidating
# them; closed must still suppress, because gc doctor reads the canonical
# checkout and keeps warning for the whole branch->merge window.
test_suppressed_by_closed_alert() {
    assert_suppressed_by "suppressed_by_closed_alert" "closed" "[\"$PATROL_LABEL\"]"
}

# Regression (ga-xw25): observed in the live ledger — the consolidation bead
# ga-4fbf sat in `deferred`, which neither `open` nor an open+in_progress
# widening would have matched.
test_suppressed_by_deferred_alert() {
    assert_suppressed_by "suppressed_by_deferred_alert" "deferred" "[\"$PATROL_LABEL\"]"
}

test_suppressed_by_blocked_alert() {
    assert_suppressed_by "suppressed_by_blocked_alert" "blocked" "[\"$PATROL_LABEL\"]"
}

# Regression (ga-xw25): the consolidation bead carries census.owner_bead but
# not the patrol's own label, so a label-scoped query cannot see the very
# bead that is doing the work.
test_suppressed_by_unlabelled_consolidation_bead() {
    assert_suppressed_by "suppressed_by_unlabelled_consolidation_bead" "in_progress" "[]"
}

# ---------------------------------------------------------------------------
# The dedup must not over-suppress: it keys on the owner_bead, not on the
# mere existence of some census alert somewhere.
# ---------------------------------------------------------------------------

test_files_alert_for_different_owner_bead() {
    local env out rc
    env="$(new_env)"
    set_doctor_warning "$env" "$DANGLING"
    add_bead "$env" "ga-unrelated" "open" "ga-someotherbead" "[\"$PATROL_LABEL\"]"

    out="$(run_check "$env")"; rc=$?

    if [ "$rc" -ne 0 ]; then
        record_fail "files_alert_for_different_owner_bead" "exit $rc, want 0: $out"
        return
    fi
    if [ "$(created_owner_beads "$env")" != "ga-80po0c.2" ]; then
        record_fail "files_alert_for_different_owner_bead" \
            "filed for [$(created_owner_beads "$env")], want ga-80po0c.2 — an unrelated owner_bead must not suppress"
        return
    fi
    record_pass "files_alert_for_different_owner_bead"
}

test_partial_suppression_across_two_dangling_beads() {
    local env out rc
    env="$(new_env)"
    set_doctor_warning "$env" "$DANGLING" "$DANGLING_OTHER"
    add_bead "$env" "ga-existing" "closed" "ga-80po0c.2" "[]"

    out="$(run_check "$env")"; rc=$?

    if [ "$rc" -ne 0 ]; then
        record_fail "partial_suppression_across_two_dangling_beads" "exit $rc, want 0: $out"
        return
    fi
    if [ "$(created_owner_beads "$env")" != "ga-c1slhq" ]; then
        record_fail "partial_suppression_across_two_dangling_beads" \
            "filed for [$(created_owner_beads "$env")], want only ga-c1slhq"
        return
    fi
    record_pass "partial_suppression_across_two_dangling_beads"
}

# Idempotence is the whole point: the second tick of a cron order over an
# unchanged condition must file nothing.
test_second_run_files_nothing() {
    local env out rc
    env="$(new_env)"
    set_doctor_warning "$env" "$DANGLING"

    run_check "$env" >/dev/null
    out="$(run_check "$env")"; rc=$?

    if [ "$rc" -ne 0 ]; then
        record_fail "second_run_files_nothing" "exit $rc, want 0: $out"
        return
    fi
    if [ "$(created_count "$env")" != "1" ]; then
        record_fail "second_run_files_nothing" \
            "two ticks filed $(created_count "$env") beads, want 1: $out"
        return
    fi
    record_pass "second_run_files_nothing"
}

# ---------------------------------------------------------------------------
# Detection-only guarantees that already held; pinned so the dedup rewrite
# cannot regress them.
# ---------------------------------------------------------------------------

test_no_warning_files_nothing() {
    local env out rc
    env="$(new_env)"
    printf '%s' '{"results":[{"name":"census-owner-liveness","status":"ok","details":[]}]}' \
        > "$env/state/doctor-json"

    out="$(run_check "$env")"; rc=$?

    if [ "$rc" -ne 0 ] || [ "$(created_count "$env")" != "0" ]; then
        record_fail "no_warning_files_nothing" "exit $rc / created $(created_count "$env"): $out"
        return
    fi
    record_pass "no_warning_files_nothing"
}

test_warning_without_dangling_findings_files_nothing() {
    local env out rc
    env="$(new_env)"
    set_doctor_warning "$env" "skipped 3 rows with no owner_bead column"

    out="$(run_check "$env")"; rc=$?

    if [ "$rc" -ne 0 ] || [ "$(created_count "$env")" != "0" ]; then
        record_fail "warning_without_dangling_findings_files_nothing" \
            "exit $rc / created $(created_count "$env"): $out"
        return
    fi
    record_pass "warning_without_dangling_findings_files_nothing"
}

# gc doctor exits nonzero when unrelated BLOCKING checks fail; the wrapper
# must still process its own advisory findings.
test_tolerates_nonzero_doctor_exit() {
    local env out rc
    env="$(new_env)"
    set_doctor_warning "$env" "$DANGLING"
    printf '1' > "$env/state/doctor-exit"

    out="$(run_check "$env")"; rc=$?

    if [ "$rc" -ne 0 ] || [ "$(created_count "$env")" != "1" ]; then
        record_fail "tolerates_nonzero_doctor_exit" "exit $rc / created $(created_count "$env"): $out"
        return
    fi
    record_pass "tolerates_nonzero_doctor_exit"
}

test_invalid_doctor_json_fails_loudly() {
    local env out rc
    env="$(new_env)"
    printf 'not json at all' > "$env/state/doctor-json"

    out="$(run_check "$env")"; rc=$?

    if [ "$rc" -eq 0 ]; then
        record_fail "invalid_doctor_json_fails_loudly" "exit 0, want nonzero: $out"
        return
    fi
    if [ "$(created_count "$env")" != "0" ]; then
        record_fail "invalid_doctor_json_fails_loudly" "filed $(created_count "$env") beads on bad input"
        return
    fi
    record_pass "invalid_doctor_json_fails_loudly"
}

test_missing_check_fails_loudly() {
    local env out rc
    env="$(new_env)"
    printf '%s' '{"results":[{"name":"some-other-check","status":"ok","details":[]}]}' \
        > "$env/state/doctor-json"

    out="$(run_check "$env")"; rc=$?

    if [ "$rc" -eq 0 ]; then
        record_fail "missing_check_fails_loudly" "exit 0, want nonzero: $out"
        return
    fi
    record_pass "missing_check_fails_loudly"
}

# ---------------------------------------------------------------------------

echo "check-census-owner-liveness:"
test_files_alert_when_untracked
test_suppressed_by_open_alert
test_suppressed_by_in_progress_alert
test_suppressed_by_closed_alert
test_suppressed_by_deferred_alert
test_suppressed_by_blocked_alert
test_suppressed_by_unlabelled_consolidation_bead
test_files_alert_for_different_owner_bead
test_partial_suppression_across_two_dangling_beads
test_second_run_files_nothing
test_no_warning_files_nothing
test_warning_without_dangling_findings_files_nothing
test_tolerates_nonzero_doctor_exit
test_invalid_doctor_json_fails_loudly
test_missing_check_fails_loudly

echo
echo "passed: $pass, failed: $fail"
[ "$fail" -eq 0 ] || exit 1
