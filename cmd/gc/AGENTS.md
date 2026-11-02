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
