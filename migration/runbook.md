# Full-Instance Cutover Runbook (Laravel -> Standalone Astro + Vue 3.5 + Go + Rust)

1. Take verified PostgreSQL (`pg_dump`), Redis snapshot, `APP_KEY`, and `/data/coolify` backup in maintenance window.
2. Stop legacy Laravel containers (`coolify`, Horizon, Reverb, Scheduler, `coolify-terminal`) to ensure single-writer fencing.
3. Run `coolify-migrate --dry-run --cutover-preflight` to verify row counts, `id = 0` sentinels, `APP_KEY` decryptability, keyed digests, and active remote operation journals.
4. Boot standalone `docker/stack-migration/docker-compose.cutover.yml` (`controlplane`, `worker`, `executor`, `postgres`, `redis`) using a fresh Go Redis key namespace.
5. Verify `/api/v1/health`, `/api/v1/servers`, SSE logs, WebSocket terminal, and reconcile any `running`/`unknown` operations before resuming cron schedules.
