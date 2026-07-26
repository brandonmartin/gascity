---
title: The Fork-to-Upstream Publish Debt
description: Why merging a bead to develop is not the end of its life on a fork, which component owes the upstream PR, and the metadata contract that makes the debt visible instead of silent.
---

## Why this exists

On 2026-07-26 this fork was measured at 58 commits and 15 days past its last
upstream PR, with zero open upstream PRs. Nothing had failed. No agent had
erred. The pipeline ran exactly as designed and the design stopped at
`origin/develop`.

That is the defect this page exists to prevent recurring: **a fork accumulates
divergence silently, because "merged" and "published" are different states and
only one of them was ever represented.**

## The two-state truth

A bead on a fork passes through two terminal states, not one:

| State | Who moves it | Recorded where |
|---|---|---|
| **merged** — the change is on `develop` | refinery, `merge-push` | `merged_sha`, `merged_target`, bead closed |
| **published** — the change is offered upstream | the accountable publisher | *was recorded nowhere* |

Only the first was ever written down. A closed bead therefore looked
indistinguishable from a finished one, so the second state had no queue, no
age, and no owner — and grew to 58 commits without a single alarm.

## The refinery enqueues; it does not publish

The obvious fix — have the refinery call `gh pr create` — is wrong, and
deliberately so. See [Publish-Gate Staleness](publish-gate-staleness.md): Gas
Town splits irreversible publishes. A worker produces a verified branch and
halts; a single accountable actor performs the push. That split keeps
force-pushes and upstream-PR updates behind one reviewer.

So the refinery's job at merge time is not to publish. It is to **make the debt
exist as a durable object**:

> When a rig has an `upstream` remote and a merged change is not already
> upstream by patch-id, closing the work bead must also open a publish bead
> carrying the publish-gate metadata contract.

That single rule is the whole fix. It feeds the meter `ga-qbq` already built
(`internal/publishgate`, `gc bd publish-gate`, the `gc doctor` check), which
until now had nothing to measure because nothing ever enqueued.

The formula-text change that implements this is owned by the packs repo, not
this one — it lives in `mol-refinery-patrol.toml`'s `merge-push` step. Do not
fork that text into this repository.

## What a publish bead carries

The keys are the existing publish-gate contract, so the existing reader works
unchanged:

| Key | Value |
|---|---|
| `branch_ready` | `true` — this bead holds a finished, unpublished artifact |
| `branch_ready_at` | stamped at the write; **this is the clock** |
| `commit` | the merge SHA on the fork's integration branch |
| `target` | `upstream/main` — remote-qualified, so it is unambiguous |
| `source_bead` | the closed work bead this debt came from |
| `halt_reason` | `mayor_publish_gate` |

## Draining one

`metadata.commit` is a merge commit on `develop`. **It is not cherry-pickable
upstream as-is.** The drain is:

```bash
git fetch upstream
git checkout -b pr/<slug> upstream/main     # never off main or develop
git cherry-pick -x <the source bead's own commits>
```

Three rules, each of which cost real time to learn:

1. **Drop fork-local bookkeeping from the PR.** `internal/testpolicy/resourcecensus`
   and `TESTING.md` ratchet commits are fork-side accounting. They are also the
   most common cherry-pick conflict — a conflict there is a signal the commit
   should not be in the PR at all, not a merge to resolve.
2. **A clean cherry-pick is not a compiling one.** Fork test files routinely
   reference helpers that do not exist upstream. Expect to adapt test
   scaffolding even when the fix itself applies untouched.
3. **Patch-id dedupe is necessary, not sufficient.** `git cherry` only catches
   byte-identical history. It cannot see an upstream fix written differently,
   which is the likeliest way a stale fork delta is already obsolete. Audit by
   content — read the upstream call site and ask whether the bug is still there
   — before filing anything.

## The invariant to check

If `git rev-list --count upstream/main..origin/develop` is large while
`gc bd publish-gate` is empty, the enqueue is broken again. Those two numbers
are supposed to move together.
