# Controlled Rollback Playbook (Standalone Go/Rust -> Frozen Laravel Baseline)

1. Run `coolify-migrate --cutover-preflight` to assert `VerifyRollbackCompatibility()` passes (expand-only schema invariant verified, no destructive schema changes accepted).
2. Stop Go `controlplane` and `worker` cleanly; preserve remote `/data/coolify/operations/<operation_id>/journal.json` files for all `running`/`unknown` jobs (never delete journals to force retries).
3. Flush Go-only Redis namespace (`coolify:go:v1:*`) so frozen Laravel caches recompute cleanly from PostgreSQL.
4. Start the frozen baseline Laravel containers against the intact PostgreSQL database and verify health checks and schedules.
5. If post-cutover destructive writes occurred that break rollback compatibility, require explicit operator authorization for snapshot restoration (declaring discarded post-cutover writes).
