# CLI and supervisor implementation

Pack-command dispatch consumes root-owned `--city` and `--rig` before a
leaf, exports the resolved city and rig to the child environment, and passes
the leaf's own arguments unchanged. Keep eager and lazy discovery equivalent.

Pool creates prove availability under the complete city-scoped identifier lock
set. The cross-store census must be live and complete. Within that fence,
primary alias, runtime-name and identity checks share a fresh type+label session
union; closed-name reservations use exact `session_name` probes, never a scan of
all closed history. A partial read refuses creation. Keep the real write store
separate from this per-create availability reader so ID minting and conditional
writes retain the provider's semantics.

Session availability storage adapters belong at the per-class composition edge
(`cmd/gc/class_store.go`); the desired-state builder keeps using session-owned
name checks. Keep API publication and full city readiness as separate barriers;
see `internal/api/AGENTS.md`.

Order-tracking retention maintenance commits at most one guarded delete chunk
per pass. Independent guards run with bounded concurrency; all guard reads finish
before that chunk is deleted. Preserve live-member, parent-child and dependent
checks, including the full traversal fallback for non-leaves. Store errors keep
the affected root intact, and cancellation flushes only roots already guarded.

Failure-event excerpts omit routine startup configuration notices before
truncation. Keep complete redacted output in the supervisor log and preserve the
execution error, including the effective timeout, when no diagnostic remains.
