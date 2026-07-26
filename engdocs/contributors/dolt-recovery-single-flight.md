# Managed Dolt recovery: the single-flight contract

Managed-Dolt recovery is the only Gas City path that deliberately kills the
process every agent depends on. Recovery running when it should not is worse
than recovery not running at all: it converts a transient overload into a
sustained outage. This documents the invariants that keep it single-flight, and
why each one exists.

The code lives in `cmd/gc/dolt_recover_managed.go`
(`recoverManagedDoltProcessWithOps`), reached from the shell provider's
`op_recover` in `examples/bd/assets/scripts/gc-beads-bd.sh` and from
`healthBeadsProviderContext` in `cmd/gc/beads_provider_lifecycle.go`.

## Why the guards are layered

Recovery is triggered by *clients*, not by a supervisor. Every `gc` invocation
whose bead-store call fails may decide to recover, and each one is a separate
OS process. So the guards have to work across processes:

| Guard | Scope | Where |
|---|---|---|
| Recover cooldown (`lastBeadsProviderRecover`) | one process | `beads_provider_lifecycle.go` |
| Provider semaphore | one process | `acquireProviderSemaphoreForOpContext` |
| Lifecycle flock (`<pack-state>/dolt.lock`) | whole host | `dolt_lifecycle_lock.go` |

Only the flock survives the fan-out. The first two shape a single agent's
retry behavior; they do nothing about ten agents failing at once. Do not add a
new in-process guard and call the herd solved.

## Invariant 1: a queued recovery adopts, it does not restart

The flock serializes recoveries, which is necessary but not sufficient. Serial
recoveries are still N recoveries: when the winner releases the lock, the next
waiter acquires it, finds the freshly started server not yet answering — a
managed city's server has to open every database before it serves — and stops
it. Repeat per waiter. That is a crashloop built entirely out of correctly
serialized recoveries, and it is what turned a 90-second connection overload
into a 7.5-minute town-wide outage across six server generations (`ga-oigp`).

`recoverManagedDoltAdoptRunningServer` closes it. When the lock was acquired
only after waiting on another holder (`queued`), a failed probe is treated as
"still starting", not "wedged": recovery re-probes until the caller's deadline
and adopts the server if it becomes query-ready.

Two deliberate exits from that wait:

- **Nothing is listening** (`managedDoltRecoverListenerReachableFn` is false) —
  there is no replacement to wait for, so restart immediately. This keeps a
  genuinely failed winner from costing the next process its whole budget.
- **The server is read-only** — never a warm-up state. Diagnose and restart on
  the first probe, exactly as a first-arrival recovery does.

A first-arrival recovery (`queued` false) probes once and restarts, unchanged.
The settle applies only where the herd forms.

## Invariant 2: recorded runtime state is not evidence of a live process

`dolt-state.json` and `dolt-provider-state.json` are written at start and are
not retracted when the process dies. During the `ga-oigp` outage the published
state advertised a pid that had been dead for minutes as `"running": true`, for
the entire window.

Anything that reads those files as a *recovery input* must verify the pid
first. `recoverManagedDoltObservedRebindPossible` and
`recoverManagedDoltPopulateReportFromRuntimeState` both take an `alive`
predicate for this. The concrete failure without it: a stale entry naming a
different port made recovery conclude another generation had rebound
elsewhere, and one racer went off to a port nobody was serving.

The same rule already holds elsewhere — `validDoltRuntimeState` and
`managedDoltExistingStateMatches` check `pidAlive` — so match that pattern
rather than inventing a new one.

## Invariant 3: cap exhaustion must not be the backpressure mechanism

`listener.max_connections` is not a pool size. Every bd/dolt-sql operation
opens its own short-lived connection, and one whose client exits without a
clean `COM_QUIT` sits in Sleep until `read_timeout_millis` reaps it, so live
connections track *arrival rate x read timeout*. A busy fleet sits in the
hundreds with nothing wrong.

Hitting the cap is not graceful degradation: it fails every reader and writer
at once, which is precisely the condition that makes every agent call recovery.
`config.DefaultDoltMaxConnections` is therefore set above the expected peak,
and overload is absorbed by the read timeout instead. Cities tune it via
`city.toml` `[dolt] max_connections`.

That default is duplicated in two shell fallbacks, because `gc` only exports
`GC_DOLT_MAX_CONNECTIONS` when the value is set explicitly:
`gc-beads-bd.sh` (writes the config the server binds) and `mol-dog-doctor.sh`
(sizes the near-capacity advisory). `TestDoltMaxConnectionsFallbacksMatchManagedDefault`
pins both to the Go constant.

## When you change this code

- Reproduce the herd, not just the single-process path. The end-to-end shape is
  `TestRecoverManagedDolt_WaiterDoesNotRestartWarmingServer`: hold the
  lifecycle flock from a second fd, release it mid-recovery, and assert no
  stop/start ran.
- Keep the diagnosis in the probe results, not in Go heuristics. "Not answering
  yet" versus "wedged" is decided by re-probing against a deadline, not by
  guessing from elapsed time or restart counts.
- A recovery that returns without restarting is a success, not a no-op. Report
  `ready`/`healthy` so the caller does not immediately try again.

## Related

- `engdocs/contributors/dolt-maintenance.md` — scheduled compaction and snapshots
- `engdocs/contributors/dolt-regression-audit.md` — prior Dolt-path regressions
- `AGENTS.md`, "Dolt Server" — diagnostics to collect *before* any restart
