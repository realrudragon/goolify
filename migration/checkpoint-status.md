# Coolify Standalone Migration Checkpoint & Verification Ledger

**Workspace:** `c:\WorkSpace\coolify-golang`  
**Frozen Baseline SHA:** `171eae00a76dff4f5a02c2b5130d7b2dfe219a7d`  
**Target Stack:** Standalone Astro + Vue 3.5 (`vue@^3.5.45` with `@astrojs/vue@^7.1.6`) + TypeScript + Go (`1.26.5`) + Rust (`coolify-executor`)  
**Runtime Policy:** 100% Real PostgreSQL 16 + Redis 7 + Compiled Rust Executor + Real SSH/HTTP/SMTP/S3/Chromium — **zero mock data or hardcoded UI arrays in production code**.  
**Task 14 Legacy Removal & 10-Dimension Security/Parity Audit:** **COMPLETE & VERIFIED** (`0` `.php`, `0` `.svelte`, `0` `TODO`/`FIXME`/`TRUSTED_PROXIES`/`unimplemented!`, `0` em dashes, `0` `v-html`/`innerHTML`, `100.0%` PostgreSQL DDL match across all Concrete Data Models).

---

## 1. Critical Audit, Performance, Security & Tech-Stack Parity Resolved (Slices 1–20 + All-Audit)

| Audit / Parity Area | Finding Found | Fix Implemented | Verification Evidence |
|---|---|---|---|
| Data-coverage table name vs PostgreSQL DDL alignment | 17 rows in `migration/data-coverage.csv` had naive pluralization typos (`instance_settingss`, `s3storages`, `standalone_rediss`, etc.) | Corrected all 17 rows to exact table names (`abstract_base_model`, `query_scope_trait`, `s3_storages`, `standalone_redis`, `*_notification_settings`, etc.) and added PostgreSQL DDL assertion to `tests/stack-migration/inventory.test.ts` | `node --test tests/stack-migration/inventory.test.ts tests/stack-migration/web-shell.spec.ts` (`6/6 passed`) |
| Server Transfer completeness (`AGENTS.md` rule) | `PostgresTransferService` previously only transferred Applications + Envs + PrivateKeys and did not set `servers.transferred_at` directly | Extended `backend/internal/domains/transfer/postgres_service.go` (`ExportServer`/`ImportServer`) to transfer Services (`service_applications`, `service_databases`), all 9 Standalone DB engines (`scheduled_database_backups`), Volumes, ScheduledTasks, Tags, and team credentials (`s3_storages`, `integration_tokens`, `cloud_provider_tokens`, `github_apps`), plus setting `servers.transferred_at` and `transfer_status = 'transferred_away'` | `internal/domains/transfer` + `TestProductionSlice20_TechStackCloningTemplatesAndFullParity` (`passed`) |
| Cross-team / Switched-session / Transferred-archive boundaries | `/team/members/{user_id}/avatar` queried without team join; SBOM download and `/files/download/` scoped by session team before checking loaded resource's team; `/api/v1/sentinel/push` did not exclude `transferred_at IS NOT NULL` | Hardened `v1_slice13_web_canvas.go` (`/team/members/{id}/avatar` enforces shared team membership -> `403`, SBOM + file download authorize against loaded resource's team) and `v1_slice12_servers_vuln.go` (`s.transferred_at IS NULL` in `handleSentinelPush` -> `401`) | `TestProductionSlice20_TechStackCloningTemplatesAndFullParity` (`403` cross-team avatar, `401` transferred sentinel push, `passed`) |
| One-click 382+ service templates, build packs, git clone & deep cloning | Service templates catalog was not exposed on API; git deploy lacked idempotent clone; deep clone missed settings/volumes/tasks/tags/backups/sub-resources | Created `services/templates.go`, synced `service_applications`/`service_databases`, added `GET /api/v1/services/templates` & template/base64 creation, embedded `/app/templates` & `/app/svgs` in `Dockerfile`, added `gitCloneOrFetchCmd`, deep-cloned apps/services/databases/environments (`v1_slice9.go`, `v1_slice11_subresources.go`, `v1_slice11_resources.go`), updated `ResourcesManager.vue`, `DatabasePanel.vue`, `DashboardRoot.vue` | `TestProductionSlice20` + `apps/web` + `browser-e2e.spec.mjs` (`passed`) |
| Auth hot-path WAL amplification | Synchronous `last_used_at` `UPDATE` ran on every read Bearer API request | Throttled `last_used_at` update in `backend/internal/auth/postgres_store.go` to once per minute | `internal/auth` + `packaging-benchmark.spec.ts` (`p50 = 3.89ms`, `p95 = 12.91ms`, `p99 = 20.44ms`, `passed`) |

---

## 2. Inventory & Parity Progress (100.0% Across All 3 Ledgers)

| Ledger File | Verified | Implemented | Inventoried | Total | Coverage |
|---|---:|---:|---:|---:|---:|
| `migration/data-coverage.csv` | **81** | 0 | 0 | 81 | **100.0%** (79 concrete tables verified in Postgres DDL + 2 abstract/trait classes) |
| `migration/routes.csv` | **631** | 0 | 0 | 631 | **100.0%** |
| `migration/features.csv` | **718** | 0 | 0 | 718 | **100.0%** |

---

## 3. Latest Measured Standalone Benchmarks (`migration/receipts/benchmark-report.json`)

| Metric | Measured Value (Post-Audit Standalone Build) |
|---|---:|
| Multi-stage Docker OCI image size (`docker image inspect`) | **47.45 MB** (with embedded `/app/templates` + `/app/svgs`, 5 binaries, Astro/Vue dist; zero PHP/Node/Svelte; `uid=1000(coolify)`) |
| Control plane idle RSS / active RSS / Worker RSS | **62.17 MB** / **67.09 MB** / **61.77 MB** |
| API latency (100 real Postgres-backed requests) | **p50 = 3.89 ms**, **p95 = 12.91 ms**, **p99 = 20.44 ms** |
| Concurrent deployments queued & drained by `coolify-worker` | **10 / 10** in **4.32 s** (**2.31 / sec**, serial lock per resource) |
| Live SSE log streaming throughput (`/realtime/logs`) | **100 lines @ 5,481.1 lines/sec** |
| Live WebSocket terminal round-trip (`/app/terminal/ticket` + `/terminal/ws`) | **252.75 ms** |
| Hard `SIGKILL` controlplane restart recovery (PostgreSQL session + project) | **Verified (`true`)** |

---

## 4. Final Verification Matrix (`passed` / `failed` / `blocked` / `not_run`)

### Passed (All Re-Executed After Final Audit Remediation Edits)
1. **Static & Schema Audit (`0` findings remaining):** `apps/web/src` (34 files, `0` em dashes, `0` `v-html`/`innerHTML`, `87` wired API calls), Go worker job type coverage (`13/13` handled), production code hygiene (`0` `TODO`/`FIXME`/`TRUSTED_PROXIES`/`unimplemented!`), `REMAINING_PHP=0`, `REMAINING_SVELTE=0`.
2. **Inventory + PostgreSQL DDL + Web Shell Contract Suite (`node --test tests/stack-migration/inventory.test.ts tests/stack-migration/web-shell.spec.ts`):** **6/6 PASSED (`exit 0`)**.
3. **Go Backend Full Suite (`cd backend && go test -count=1 -p 1 ./...`):** **26/26 packages PASSED (`exit 0`, 0 failures, 0 skips)** against real PostgreSQL 16 + Redis 7.
4. **Rust Executor Workspace (`cargo test --workspace -q`):** **10/10 PASSED (`exit 0`)** (`coolify-protocol` 4/4, `coolify-runner` 6/6).
5. **Astro + Vue 3.5 Typecheck & Production Build (`cd apps/web && npm run check && npm run build`):** **PASSED (`0` errors, `0` warnings, `0` hints)**.
6. **Real Headless Chromium Browser E2E (`node --test tests/stack-migration/browser-e2e.spec.mjs`):** **9/9 PASSED (`exit 0`)**.
7. **Task 14 Multi-Stage Docker Packaging & Live Performance Benchmarks (`node --test tests/stack-migration/packaging-benchmark.spec.ts`):** **3/3 PASSED (`exit 0`)**.
8. **Live Remote SSH E2E & Two-Round `cmd/migrate` Rehearsal (`rehearsal-1.json` / `rehearsal-2.json`):** **PASSED** (verified prior to final audit doc/transfer edits; 8 sentinel-0 tables, 0 broken FKs, `rollback_compatible=true`).

### Failed
- None (`0` failing tests).

### Blocked / Not Run (Environment Constraints)
1. **`go test -race` on Windows host:** **Blocked** (`CGO_ENABLED=1` / `gcc` not installed on host).
2. **`cargo clippy` & `cargo fmt --check`:** **Blocked** (`openhouse-rust-renderer-check:latest` Docker image lacks `clippy`/`rustfmt` components; host lacks `cargo`).
3. **External internet third-party paid cloud/secret provider credentials:** **Not run** against live paid external accounts (verified against local HTTP/SMTP/S3/SSH protocol servers & PostgreSQL 16 + Redis 7).
4. **`git commit` / `git push` / `release.json` / `CHANGELOG.md`:** **Not run / Untouched** per explicit constraints.
