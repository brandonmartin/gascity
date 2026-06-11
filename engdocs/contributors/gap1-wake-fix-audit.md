# Gap-1 wisp-wake fix: completeness audit

**Bead:** ga-082 (follow-up) · **Parent:** ga-n2d (still open) · **Date:** 2026-06-11

This audit answers the two reviewable asks on ga-082 — (1) review the
`excludePatrolWakeWisps` deviation for semantic equivalence, and (2) decide how
to land the five upstream #2675 hardening commits — and reports a blocking
finding discovered while grounding those asks against current code.

All claims below were verified against the working tree at the date above.
Confirm against current `git` state before acting; this fork has repeatedly lost
working code during branch churn (see Finding 1), so a doc is evidence, not
gospel.

---

## TL;DR

| # | Finding | Status |
|---|---------|--------|
| **F1** | **The gap-1 wisp-wake core has regressed off `develop`.** The port (`185592653`, introducing the wake probe + `excludePatrolWakeWisps`) is on **no** `develop` tip — only on `polecat/ga-80g\|841\|s69`. ga-082's "land the hardening commits" ask is **blocked** until the core is restored. | **Blocking — escalated to `ga-n2d`** |
| **F2** | The `excludePatrolWakeWisps` deviation **is semantically equivalent to upstream's strict issue-tier path for the wake path**, by construction: one predicate (`isPatrolWispWakeCandidate`) gates both the dedicated probe and the generic-demand exclusion. Two bounded residual risks documented. | **Review complete** |
| **F3** | **Decision: option (c)** — defer the five hardening commits until #2675 lands upstream, with a partial-(b) escape hatch for the two self-contained nudge commits. **Reject (a).** The #1600 read-model entanglement is isolated in the stack-base commit `c67b3631b` only. | **Decided** |

---

## Finding 1 (blocking): the gap-1 core regressed off `develop`

ga-082's description asserts the core is "deployed on develop @185592653 and
live." That is **stale.** The port is absent from every current `develop` tip.

### Evidence

Commit map:

| Ref | Commit | Carries the gap-1 wake port? |
|-----|--------|------------------------------|
| Upstream #2675 core | `f7d051b19` | n/a (upstream) |
| Fork port (initial) | `92fbd15e1` | yes — adds probe (`build_desired_state.go +124`, test `+341`) |
| Fork port (single-tier + deviation) | `185592653` | yes — adds `excludePatrolWakeWisps` + `listForControllerDemand` (`+60/-6`) |
| **`origin/develop`** | `edd004a08` (ga-n2d.2) | **NO** |
| **live `develop`** (`/home/b/GIT/gascity`) | `a2325984b` | **NO** |
| `polecat/ga-80g`, `polecat/ga-841`, `polecat/ga-s69` | contain `185592653` | yes |

Verification (all reproducible):

```
git merge-base --is-ancestor 185592653 origin/develop  → NO
git merge-base --is-ancestor 185592653 a2325984b       → NO   (live develop)
git show origin/develop:cmd/gc/build_desired_state.go | grep -c excludePatrolWakeWisps          → 0
git show origin/develop:cmd/gc/build_desired_state.go | grep -c namedSessionPatrolWispWakeDemand → 0
# live develop on-disk: namedSessionPatrolWispWakeDemand → absent
git log --all -S excludePatrolWakeWisps -- cmd/gc/build_desired_state.go
  → 185592653 (only)                       # introduced once; never re-applied to current develop
git log --all --grep=revert -i | grep -i 'wisp\|wake\|patrol'
  → (none)                                 # no deliberate revert — lost in a rebuild, not removed
```

### Topology (why it was lost)

Three `develop` lineages have diverged:

```
                         91e64b9a1 (#3064)         71fc74323 (#3262)
live develop  a2325984b  ──+2──┐                        │
origin/develop edd004a08 ──────┴──────────+34───────────┤  (+37 from #3262 base)
port lineage  185592653  ───────────────────── +9 from #3262 base ┘  (polecat/ga-80g…)
```

- `origin/develop` (the base polecats branch from) is **+34** beyond the live
  checkout; the live checkout is effectively a stale `develop`.
- The port (`185592653`) was built on an **older** `develop` snapshot. When
  `develop` was rebuilt onto newer upstream main, the individual bead branches
  (ga-80g/ga-841/ga-n2d.x) were re-landed under fresh hashes, but the gap-1
  wisp-wake port was **not re-applied** and silently dropped. No revert commit
  exists. This is the exact failure mode AGENTS.md warns about
  ("repeatedly lost working code during branch churn").

### Scope note: only gap-1 regressed

Gap-2 (#3311 state-cache, the `PoolDesiredCounts` demand snapshot) **is** present
on `origin/develop` (`grep -c PoolDesiredCounts → 1`). The parent bead `ga-n2d`
("asleep refinery/witness don't wake on work; town-wide merge-stall risk") is
**still open**. So the underlying gap-1 defect ga-082 builds on is, in reality,
**unfixed on `develop`** — the running controller (built from live `develop`)
does not contain the named-session patrol-wisp wake.

### Consequence for ga-082

"Land the five #2675 hardening commits" cannot proceed: they refine a wake
mechanism that is absent from `develop`. **Restoring the core is a prerequisite
and belongs to `ga-n2d`**, not this follow-up — and it requires an operator
decision on which `develop` is canonical (the three tips above must be
reconciled first). Escalated via nudge; do not restore the core under ga-082
across an ambiguous base.

---

## Finding 2 (review complete): the `excludePatrolWakeWisps` deviation

### What upstream does vs. what the fork does

- **Upstream #2675 ("strict issue-tier-only generic path"):** generic controller
  demand reads the **issue tier only**, so *all* wisps are excluded from generic
  demand. Patrol wisps wake their named on_demand session through a dedicated
  wisp-tier probe.
- **Fork (`185592653`):** generic demand keeps reading **both tiers**
  (`listBothTiersForControllerDemand`) and wraps each append site
  (in_progress / open-routed / ready) in `excludePatrolWakeWisps(...)`, dropping
  *only* patrol-wake candidates while **retaining** non-patrol wisps (pool-routed
  work, control-dispatcher retry handoffs). The named-session direct-demand match
  also gains `if wb.Ephemeral { continue }` so no wisp can wake a named session
  outside the gated probe.

The fork cannot adopt the strict issue-tier path because two pre-existing fork
tests require in-progress/ready ephemeral pool/retry wisps to **stay** in generic
demand: `TestCollectAssignedWorkBeadsIncludesAssignedInProgressWisp` and
`...IncludesReadyOpenAssignedWisp` (upstream dropped both).

### Why it is equivalent for the wake path — by construction

The **same predicate** gates both sides:

```go
func isPatrolWispWakeCandidate(b beads.Bead) bool {
    return b.Ephemeral &&
        (b.Status == "open" || b.Status == "in_progress") &&
        anyOf(b.Title, b.Ref, b.Metadata["formula"], b.Metadata["gc.formula"],
              b.Metadata["wisp_type"], b.Metadata["gc.wisp_type"], isPatrolFormulaSignal)
}
```

- `namedSessionPatrolWispWakeDemand` wakes an identity **iff** one of its assigned
  wisps satisfies `isPatrolWispWakeCandidate`.
- `excludePatrolWakeWisps` removes from generic demand **exactly** the beads that
  satisfy `isPatrolWispWakeCandidate`.

So the set removed from generic demand **equals** the set the dedicated probe
owns — no gap, no overlap. The divergence from upstream (retaining non-patrol
wisps in generic demand) is the **intentional, test-pinned** fork behavior and is
**orthogonal to the wake path**: it cannot affect whether a patrol session wakes.

### Adversarial checks

| Attack | Result |
|--------|--------|
| **Double-wake** (patrol wisp counted in generic demand *and* the probe) | Prevented — excluded at all three append sites; drives the probe only. |
| **Pre-emption** (non-patrol wisp wakes a named session before the gate) | Prevented — `if wb.Ephemeral { continue }` skips all wisps in the named-session direct match. |
| **Under load** (many mixed patrol + pool wisps) | Filter is per-bead O(n), deterministic — volume-independent. See residual risk R2. |

### Residual risks (bounded)

- **R1 — predicate false-negative.** `isPatrolFormulaSignal` matches only
  `"patrol"`, `*-patrol`, `*.patrol`. A patrol formula not following that naming
  (in any of the six checked fields) would be **neither** excluded from generic
  demand **nor** woken by the probe → it surfaces as generic demand. Net effect:
  the fork is *more eager to wake* than upstream (upstream's tier filter would
  drop it entirely, possibly bypassing a wake). For current patrol formulas
  (`mol-refinery-patrol` → matches `-patrol`) there is no false negative.
  **Recommended guard:** a test asserting every configured patrol formula
  satisfies `isPatrolFormulaSignal`, so renames can't silently break the coupling.
- **R2 — probe `Limit` window.** `namedSessionPatrolWispWakeDemand` lists wisps
  with `Limit: namedSessionWispWakeProbeLimit`. If one identity accumulates more
  than that many wisps and the patrol wisp sorts outside the window, the probe
  could miss it. This is **pre-existing in the probe and not introduced by the
  deviation** — but it is the real "under load" caveat to watch.

**Verdict:** equivalent for the wake path in the common (recognized-formula)
case; divergence is confined to R1 (more-eager-wake on an unrecognized patrol
formula) and the pre-existing R2 limit window. Add the R1 guard test when the
core is restored.

---

## Finding 3 (decided): how to land the five #2675 hardening commits

### The stack and its #1600 entanglement

The five commits are a **linear stack** rooted at `c67b3631b` (each has it as an
ancestor). Read-model footprint:

| Commit | Subject | Touches #1600 read-model? | Other files |
|--------|---------|---------------------------|-------------|
| `c67b3631b` | harden named-session patrol wisp wake | **YES** — `order_dispatch.go +687`, `doltread/reader.go +553`, `caching_store_reads.go +127`, `bdstore.go +80` | `build_desired_state.go +80` |
| `0d1068fab` | re-fire live named sessions from wisps | no | `session_reconciler.go +86`, `build_desired_state.go +41` |
| `f5150dd44` | prefer newest wisp wake source | no | `build_desired_state.go +30` |
| `6539d5f1a` | requeue delivered patrol wisp nudges | no | `session_reconciler.go +39` (only) |
| `fc7c8e5cc` | supersede stale patrol wisp nudges | no | `session_reconciler.go +73` (only) |

The entire #1600 cached-demand read-model dependency is **isolated in the
stack-base commit `c67b3631b`**. The other four don't touch those files — but
they sit on top of `c67b3631b` and are written against its refactored APIs, so
they cannot cherry-pick cleanly without it.

### Options

- **(a) Port #1600's read-model first, then the stack cleanly.** Requires porting
  ~1300+ lines of infra (`order_dispatch +687`, `doltread/reader +553`,
  `caching_store_reads +127`) the fork has deliberately avoided. **Rejected** —
  disproportionate for five refinements and contrary to the upstream-alignment
  rule ("prefer new files / small adapters over broad edits to upstream-owned
  code"). It reintroduces exactly the entanglement the fork minimizes.
- **(b) Cherry-pick selectively.** The two `session_reconciler.go`-only commits
  (`6539d5f1a` requeue-delivered, `fc7c8e5cc` supersede-stale) are the most
  self-contained and don't need the read-model; they could be **hand-ported**
  onto the fork's current reconciler if a specific nudge bug bites. `0d1068fab` /
  `f5150dd44` touch `build_desired_state.go` wake structures that `c67b3631b`
  refactored — harder, not worth it piecemeal.
- **(c) Wait for #2675 to land upstream, then re-port the whole stack.** Zero
  local effort now; one clean, reviewed slice later. #2675 is still a **draft**
  upstream.

### Decision

**Primary: (c).** The hardening commits are **moot until the core is restored**
(Finding 1) — they polish an absent mechanism. Once the core is back and #2675
lands upstream, re-port the complete stack in one proven slice (AGENTS.md:
"port the smallest proven slice"; "help land #3311/#2675 upstream so the fork can
drop these local ports"). Keep the upstream +1 on #2675/#3311.

**Escape hatch: partial-(b).** If production shows a specific patrol-nudge defect
that `6539d5f1a` (requeue delivered nudges) or `fc7c8e5cc` (supersede stale
nudges) fixes, hand-port **that one** `session_reconciler.go` commit — it is
self-contained and read-model-independent. Do **not** pre-emptively port all five.

---

## Recommended sequencing

1. **Restore the gap-1 core onto canonical `develop` (owner: `ga-n2d`).** First
   reconcile the three `develop` tips and pick the canonical one (operator
   decision). The port is small (2 files: `build_desired_state.go`,
   `build_desired_state_test.go`) and proven on `polecat/ga-80g` @ `185592653` —
   re-apply that slice; do not reinvent.
2. **Add the R1 guard test** (configured patrol formulas ⊨ `isPatrolFormulaSignal`)
   alongside the restore.
3. **Confirm in production** that an asleep refinery auto-wakes on the next real
   merge handoff (the live proof of gap-1).
4. **Defer the five hardening commits** per Finding 3 until #2675 lands upstream;
   apply the partial-(b) escape hatch only on a concrete production trigger.
