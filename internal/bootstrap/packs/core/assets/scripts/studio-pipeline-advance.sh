#!/usr/bin/env bash
# studio-pipeline-advance — durable phase->phase dispatch for a multi-phase
# pipeline such as an AI game-dev studio (epic th-q1h). Generic: a phase opts in
# via two metadata keys, so future phases chain with no change to this script.
#
# A staged phase bead is left OPEN and UNROUTED carrying:
#   gc.pipeline_gate  = <blocker bead id>   (the prior phase; wait for it to CLOSE)
#   gc.pipeline_route = <rig>/<target>      (where to sling once the gate closes)
#
# Each cooldown tick this scans HQ and every rig for such beads. When the gate
# bead is closed it slings the bead to <target> and clears gc.pipeline_gate, so
# it fires exactly once and is safe to re-run.
#
# Why metadata gating instead of a native `blocks` dep: a cross-ledger dep fails
# open — the dependent rig's ledger cannot resolve a blocker's status in another
# rig, so a gated phase shows READY while its blocker is still open. Expressing
# the gate as metadata plus an explicit closed-status check (below) closes that
# gap. `gc bd show` is prefix-routed, so the gate bead may live in a different
# rig than the gated bead.
#
# Mechanical (status comparison + sling) — no LLM, no agent, no wisp. The
# controller runs it directly via exec.
set -uo pipefail

# `gc bd show --json` returns a single-element ARRAY; normalize to the object so
# field extraction works whether the CLI emits an array or a bare object. A bare
# `.status` / `.metadata[...]` on the array errors, which (with stderr hidden)
# reads as empty and silently stalls every advance.
first_bead_jq='if type == "array" then .[0] else . end'

advance_scope() {  # $1 = rig flag ("" for HQ, or "--rig <name>")
    local rigflag="$1" beads bead show gate route gst
    # Candidates: OPEN, UNROUTED beads opting in via gc.pipeline_gate. `gc bd
    # list` already returns an array, so `.[]` is correct here.
    # shellcheck disable=SC2086  # rigflag is an intentional word-split flag pair
    beads=$(gc bd list $rigflag --status open --json 2>/dev/null \
        | jq -r '.[]
                 | select(.metadata["gc.pipeline_gate"] != null)
                 | select((.metadata["gc.routed_to"] // "") == "")
                 | .id' 2>/dev/null) || return 0
    for bead in $beads; do
        # One read per candidate: pull the gate id and the route together.
        show=$(gc bd show "$bead" --json 2>/dev/null) || continue
        gate=$(printf '%s' "$show" | jq -r "$first_bead_jq | .metadata[\"gc.pipeline_gate\"] // empty" 2>/dev/null)
        route=$(printf '%s' "$show" | jq -r "$first_bead_jq | .metadata[\"gc.pipeline_route\"] // empty" 2>/dev/null)
        [ -z "$gate" ] && continue
        [ -z "$route" ] && continue
        # Gate-blocker status (prefix-routed, so a cross-ledger gate resolves).
        gst=$(gc bd show "$gate" --json 2>/dev/null | jq -r "$first_bead_jq | .status // empty" 2>/dev/null)
        [ "$gst" = "closed" ] || continue

        echo "studio-pipeline-advance: $bead gate=$gate CLOSED -> sling $route"
        if gc sling "$route" "$bead" >/dev/null 2>&1; then
            # Clear the gate so the bead is not reconsidered next tick. Sling also
            # sets gc.routed_to (which the discovery filter excludes), so this is
            # belt-and-suspenders for fire-once idempotence.
            gc bd update "$bead" --unset-metadata gc.pipeline_gate >/dev/null 2>&1 || true
            echo "  routed $bead -> $route (gate cleared)"
        else
            echo "  WARN: sling failed for $bead (will retry next tick)"
        fi
    done
    return 0
}

advance_scope ""   # HQ scope
# Then every non-HQ rig. `gc rig list` reports the city root as an hq=true
# pseudo-rig that `gc --rig <city>` cannot resolve, so exclude it (matches the
# orphan-sweep / cross-rig-deps `select(.hq == false)` convention).
for r in $(gc rig list --json 2>/dev/null | jq -r '.rigs[] | select(.hq == false) | .name' 2>/dev/null); do
    advance_scope "--rig $r"
done

exit 0
