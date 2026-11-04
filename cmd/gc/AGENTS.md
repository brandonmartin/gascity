# CLI and supervisor implementation

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

The v2 reconciler's fresh runtime read judges agent-process liveness using
the process names recorded at launch, not the current template. Provider configuration can
change while a session is running. If launch names cannot be read, a dead
agent process is unproven; do not recycle it as a zombie on template hints.
Fresh reads must still bracket identity and verify the same runtime object.
