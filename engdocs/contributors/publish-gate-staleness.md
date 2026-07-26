---
title: Publish-Gate Staleness
description: The metadata contract for an artifact halted at a publish gate, how gc measures the wait and the drift, and what a publisher must ship.
---

## Why this exists

Gas Town splits irreversible publishes. A worker produces a verified branch
and halts; a single accountable actor performs the push. That split is
deliberate — it keeps force-pushes and upstream-PR updates behind one
reviewer. But until `ga-qbq` the wait it creates had no clock and no
staleness measure, and the target kept moving underneath it: upstream
velocity on this fork was measured at ~38 commits/day (81 commits in 72
hours on 2026-07-26).

Three branch lineages died of that drift:

| Artifact | What happened |
|---|---|
| `rebase/develop-on-main` | abandoned 2026-06-11 at 23 behind, never published (recorded in `ga-zme`) |
| `ga-lwk` | produced 2026-07-11, needed a second full 34-commit rebase on 2026-07-25, published 2026-07-26 only after three agents escalated it |
| `ga-b5h` | rebased to 0-behind on 2026-07-11, sat 15 days, measured 529 behind on 2026-07-26 — past the 598-behind staleness that had ordered the rebase in the first place |

The gate was never slow in any single instance. It was slow because nothing
made a waiting artifact visible or urgent, and because every agent that
touched a held bead had to re-derive "how far behind is this?" by hand.

## The metadata contract

An agent halting at a gate records these keys on the work bead. `gc bd`
fills in the ones it can (see [Stamping](#stamping-happens-at-the-write)),
so a halt sequence that only sets `branch_ready` still produces a readable
artifact.

| Key | Meaning |
|---|---|
| `branch_ready` | `true` marks the bead as holding a finished, unpublished artifact. |
| `branch_ready_at` | RFC3339 instant the artifact reached the gate. **This is the clock.** |
| `branch` | The branch the work lives on. Not automatically publishable — see below. |
| `commit` | The artifact SHA. When present it is authoritative: this is what a publisher ships. |
| `target` | Where it is meant to land. Bare (`develop`) means origin's copy; remote-qualified (`upstream/main`) means that remote. |
| `target_head` | The target's SHA at the moment of the rebase, so drift-since-verification is a lookup rather than archaeology. |
| `branch_stale` | `true` when `branch` does not resolve to `commit` on a remote. |
| `halt_reason` | Why the agent stopped. A value ending in `_gate` (e.g. `mayor_publish_gate`) counts as a hold on its own. |

### `branch_ready_at` is not optional

A bead's own `updated_at` moves every time anyone touches it. `ga-b5h` had
been at the gate for fifteen days and still read as updated minutes ago.
Falling back to `updated_at` would have reported it as fresh, so a hold with
no `branch_ready_at` is reported as **age unknown** and warned about rather
than assumed young.

### `branch` is not automatically publishable

`ga-b5h`'s `metadata.branch` resolved on origin to `ec90bac29` — the
pre-rebase commit — while `metadata.commit` held the real artifact at
`f5289f710`. A publisher following the obvious field would have shipped the
wrong history and silently defeated the rebase the bead existed to perform.

Two conditions make a branch unpublishable, and both set `branch_stale`:

- the branch was never pushed, so nothing on a remote carries the artifact;
- the branch was pushed before a rebase, so the remote holds old history.

**Publish `metadata.commit`.** Treat `metadata.branch` as a label until
`branch_stale` says otherwise.

## The clock

`gc doctor` runs a `publish-gate:<rig>` check per rig. For each held bead it
reports gate age, commits-behind-target, how far the target moved since
`target_head`, the SHA to publish, and whether the branch is stale.

| Age | Result |
|---|---|
| < 24h | OK |
| ≥ 24h | Warning |
| ≥ 72h | Error |

Missing provenance (`branch_ready_at`, `target_head`, `commit`) and a stale
branch each raise the result to at least a warning regardless of age.

The check is **advisory**: a rotting artifact is a decision for the
publisher, not a reason to gate every consumer of `gc doctor`. The remedies
are to publish it, re-dispatch it for a fresh rebase, or re-stamp its
provenance — all judgment calls, so the check never auto-fixes.

## Ref resolution is local-only

`internal/publishgate` reads remote-tracking refs from the repository's own
object store and never contacts a remote. Doctor checks and CLI stamps must
stay fast, hermetic, and offline-safe, and rig repositories are fetched
constantly by worker setup.

The tradeoff is that a repository which has not fetched recently
under-reports drift. Every assessment therefore names the target tip it
measured against, so a stale measurement is visible rather than confidently
wrong. If a number looks too small, fetch the rig repo and re-run.

## Stamping happens at the write

Prose in a formula's done sequence is not a contract: it drifts, and only
the formula versions that happen to be deployed follow it. So `gc bd`
augments the write itself. When a `bd update ... --set-metadata
branch_ready=true` passes through `gc bd`, it also records
`branch_ready_at`, `commit`, `target_head`, and `branch_stale`.

The stamp is deliberately conservative:

- it only augments a write already declaring `branch_ready=true`;
- it never overwrites a key the caller set itself;
- it skips the JSON `--metadata` form, which bd refuses to combine with
  `--set-metadata`;
- every git-derived key requires the caller to be standing on the branch it
  is recording. Anyone touching the bead from another directory gets the
  timestamp and nothing else, because nothing proves which worktree they are
  in.
- any git failure omits the key rather than failing the write. A halt must
  never fail because provenance could not be resolved.

## Where the code lives

| Path | Role |
|---|---|
| `internal/publishgate` | Contract keys, assessment, git resolver |
| `cmd/gc/doctor_publish_gate.go` | The per-rig doctor check |
| `cmd/gc/cmd_bd_publish_gate.go` | The `gc bd` write-time stamp |
