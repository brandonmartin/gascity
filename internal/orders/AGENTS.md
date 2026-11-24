# Order dispatch lifetime

Create every tracked dispatch with `RunOpts.Deadline`, using the same absolute
deadline that bounds its execution context. Persist `gc.order.dispatch_deadline`
in the initial tracking-bead creation; a later update leaves a sweep race.

Stale and startup recovery sweeps must preserve a run without an outcome until
its deadline plus the completion grace period. This applies across processes
and controller reloads, so process-local in-flight counts alone are insufficient.
Reject malformed deadline metadata when selecting runs for destructive recovery.
Tracking beads without a deadline retain the legacy age/orphan recovery policy.

Push stale and retention age cutoffs into the backing List query before
hydration, across both storage tiers. Keep the stale cutoff inclusive and the
retention cutoff strict. Recheck dispatch deadlines after the narrowed read.

Keep the existing `no_work_gate` and idempotent gate-timeout overlap policies.
Ordinary orders use their open tracking bead to suppress overlapping dispatches.

# Closed tracking retention

Retention reads must push the creation cutoff into the store query, then
apply the close-time cutoff and retain at least ten closed runs per order.
Bound maintenance and watchdog deletion passes, and guard every candidate
against stranding live workflow descendants before deleting it. Live-descendant
refusals do not consume the deletion budget.

Delete the guarded set through `deleteWorkflowBeadsBatch` in chunks of at most
32 ids, so each chunk is rechecked at the set level right before its delete.
A failed chunk stops that store's pass and counts none of its cleared runs;
the per-bead fallback stops at its first failure. The cross-store bounded
sweep therefore does not charge a failing store's deletes against its budget,
and its worst case is stores times limit per pass.

Order dispatch must progress independently of tracking and retention sweeps.
Each maintenance lane owns its cadence, suspension gate, pressure episode, and
shutdown cancellation; stop and join the lanes when the runtime exits.
