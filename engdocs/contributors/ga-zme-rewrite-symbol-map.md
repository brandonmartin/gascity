# ga-zme rewrite audit: symbol map for re-flagged beads

**Bead:** ga-2an9 · **Requested by:** gascity/gastown.witness · **Date:** 2026-07-26

The `ga-zme` forward-port rewrote `origin/develop` history. A sweep of 57 closed
beads carrying `metadata.branch` found 18 whose recorded merge commit is
unreachable from `origin/develop`. After a history rewrite that is expected:
SHA-reachability and `patch-id` both misfire, so "unreachable" is not evidence
of loss.

The failure mode this page exists to prevent is the **second** one: a follow-up
audit greps for the original fix's symbol names, does not find them because the
code was refactored, and re-files a regression that never happened — or worse,
"restores" a mechanism the rewrite dropped **on purpose**.

Verify by **behaviour**, not by symbol name. This page records the mappings and
the deliberate drops that have already been established, so the next audit
starts from here instead of from a grep.

---

## Verdicts

| Bead | Original fix | Verdict on `origin/develop` @ `05af4749b` |
|---|---|---|
| **ga-5py** | pool per-instance worktree isolation | **PRESENT**, refactored and hardened. Do not re-file. |
| **ga-n2d.5** | gap-1 named-session patrol-wisp wake probe | **DROPPED ON PURPOSE.** Replaced by upstream `c4003d89b`. Do NOT restore. |
| **ga-21b** | per-agent `model` override in `city.toml` | **RESHAPED.** `config.Agent.Model` no longer exists; the 1-line test cannot compile. Do NOT restore. |

---

## ga-5py — pool per-instance worktree isolation (PRESENT)

Original fix: `47f97424f` on `fix/pool-worktree-isolation`, 17 lines in
`cmd/gc/build_desired_state.go`. `realizePoolDesiredSessions` (the bead-store
path) passed the *template* config agent straight to `resolveTemplate`, so every
pool instance expanded `{{.AgentBase}}` to the same base name and shared one
worktree. The fix gave each request a per-instance agent copy with a unique name.

That behaviour is intact. The mechanism moved from an inline block to a named
helper, and the naive `slot++` counter became real slot allocation with
collision tracking.

### Symbol map

| ga-5py symbol | Current `develop` | Location |
|---|---|---|
| `realizePoolDesiredSessions` | *unchanged* (now a 3-phase pipeline) | `cmd/gc/build_desired_state.go:2688` |
| inline `poolInstanceName` + `deepCopyAgent` + dir-qualification | extracted to `poolDesiredRequestIdentity(cfgAgent, slot) (*config.Agent, string, int)` | `cmd/gc/build_desired_state.go:3048` |
| `qualifiedInstance` (local) | *unchanged* — local in `realizePoolDesiredSessions`, plus a return value of `poolDesiredRequestIdentity` and `poolInstanceIdentity` | `build_desired_state.go:2840`, `:2847`, `:3048`, `:4794` |
| `fpExtra` | *unchanged* — `fpExtra := buildFingerprintExtra(resolveAgent)` | `build_desired_state.go:2849` |
| `resolveTemplateForSessionBead` | renamed `resolveTemplateForSessionBeadInfo` (typed `session.Info` refactor) | `build_desired_state.go:2850` |
| `tp.PoolSlot = slot` | `tp.PoolSlot = poolSlot` (and `= 0` for manual sessions) | `build_desired_state.go:2871` |
| `installAgentSideEffects(bp, cfgAgent, …)` | `installAgentSideEffects(bp, resolveAgent, …)` | `build_desired_state.go:2874` |
| `deepCopyAgent` | *unchanged* | `cmd/gc/pool.go:248` |
| `poolInstanceName` | *unchanged* | `cmd/gc/build_desired_state.go:4780` |
| naive `slot++` per request | `selectOrPlanPoolSessionBead(…, used, usedSlots)` — real slot allocation with dedup | `build_desired_state.go:2728` |

Path resolution itself also moved out of `cmd/gc` into
`internal/workdir.ResolveWorkDirPath(cityPath, cityName, qualifiedName, agent, rigs)`,
which now takes the qualified **instance** name as a parameter. That is why the
old audit heuristic ("does `cmd/gc` deep-copy the agent?") no longer answers the
question on its own.

Hardening added since ga-5py, all in the same path:

- `canonicalSessionIdentity` (`build_desired_state.go:3173`) makes the other
  pool-backed paths (rediscovery at `:2473`, store-backed dependency-floor at
  `:2615`) agree with `realizePoolDesiredSessions` on the
  `(agent, qualifiedName)` pair — all three now run the same
  `resolveAgent`/`fpExtra`/`installAgentSideEffects` shape. Divergent shapes
  across ticks trip the reconciler's config-drift drain.
- `poolInstanceIdentity` (`build_desired_state.go:4794`) refuses to mint a
  `{base}-N` name for a non-expanding agent and logs the near-miss (ga-fiw).
- `UsesCanonicalSingletonPoolIdentity()` short-circuits singleton pools back to
  the base identity instead of suffixing them.

### Behavioural evidence

`internal/workdir` carries the regression guard directly — N pool workers
sharing one `work_dir` template must each resolve to a distinct path:

```
go test ./internal/workdir/ -run 'TestResolveWorkDirPathGivesEachPoolSlotUniqueWorktree|TestResolveWorkDirPathUsesPoolInstanceBase|TestSessionQualifiedName'
ok  github.com/gastownhall/gascity/internal/workdir
```

`TestResolveWorkDirPathGivesEachPoolSlotUniqueWorktree` is the `#774` guard: it
asserts four namepool slots resolve to four distinct worktrees and fails on any
collision.

---

## ga-n2d.5 — gap-1 wisp wake probe (DROPPED ON PURPOSE)

Every symbol is absent from `origin/develop`: `isPatrolFormulaSignal`,
`isPatrolWispWakeCandidate`, `excludePatrolWakeWisps`,
`namedSessionPatrolWispWakeDemand`, `listForControllerDemand`. That absence is
**correct and intentional**, and it is already documented in-tree —
[`gap1-wake-fix-audit.md`](gap1-wake-fix-audit.md) carries a SUPERSEDED banner
added by the forward-port itself.

Upstream `c4003d89b` ("fix: wake assigned root-only molecule wisps") solved the
same problem with the **opposite** rule, and it is an ancestor of
`origin/develop`. Its `appendOpenAssignedMoleculeWorkUnique`
(`cmd/gc/build_desired_state.go:2157`) **includes** assigned root-only molecule
wisps in demand; the fork's `excludePatrolWakeWisps` **excluded** them and leaned
on a separate bounded probe. Layering the fork's filter on top of upstream would
strip precisely the beads upstream's fix exists to include.

Controller demand now reads `TierMode: beads.TierBoth`
(`listBothTiersForControllerDemand`, `readyForControllerDemandQuery`), so the
wisp tier is in demand by construction rather than via a dedicated probe.

**Therefore the 86-line `build_desired_state_test.go` slice must not be
restored.** Its `TestCollectAssignedWorkBeadsDoesNotReadWispTierForDemand` pins
the *inverse* of develop's deliberate design, and its other cases reference
runtime symbols that no longer exist — the file would not compile.

---

## ga-21b — per-agent `model` override (RESHAPED)

The original 2-line change added `Model: src.Model` to `deepCopyAgent`
(`cmd/gc/pool.go`) and `Model: "opus"` to the `TestDeepCopyAgentCoversAllFields`
fixture (`cmd/gc/pool_test.go`).

On current `develop`, **`config.Agent` has no `Model` field.** The only `Model`
in the config package is `AgentDefaults.Model`
(`internal/config/config.go:2983`), the city-level `[agent_defaults] model`
default, whose own doc comment notes it "is not yet auto-applied at runtime".
Per-agent model selection is expressed through `OptionDefaults` instead —
`option_defaults = { permission_mode = "plan", model = "sonnet" }` — and
`OptionDefaults` **is** deep-copied by `deepCopyAgent`.

So neither line is restorable and neither is needed:

- `pool.go`: `Model: src.Model` would not compile.
- `pool_test.go`: `TestDeepCopyAgentCoversAllFields` reflects over
  `config.Agent` and requires every non-tombstone field to be non-zero in the
  fixture. There is no `Model` field to cover, so the test is complete as-is;
  adding the line would not compile either.

---

## How to audit the next re-flagged bead

1. Read the original fix commit (`git show <sha>`) and write down what it makes
   *true*, not which identifiers it introduced.
2. Grep `develop` for that behaviour — the call site, the invariant, the test
   name — and widen the grep before concluding "absent". Truncated `grep | head`
   output is the single most common source of a false "NOT FOUND"; several
   symbols on the original ga-2an9 report were present all along and were missed
   exactly this way.
3. Check `engdocs/contributors/` for a superseded-banner doc before restoring
   anything. The forward-port annotated the mechanisms it dropped deliberately.
4. Run the narrowest test that pins the behaviour. Do not run a full-repo
   `./...` sweep to answer an audit question — the host is shared.
