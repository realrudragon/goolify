# Coolify Technology Stack

## Frontend

-   Astro (Static site & layout shell in `apps/web/`)
-   Vue 3.5 (`vue@^3.5.45` with `@astrojs/vue@^7.1.6`) + TypeScript
-   Tailwind CSS v4 (Design tokens per `DESIGN.md`)
-   WebSocket Terminal UI (`WebTerminal.vue`) & SSE Live Log Streaming (`LogStream.vue`)

## Backend

-   Go 1.26 (`backend/cmd/controlplane`, `backend/cmd/worker`, `backend/cmd/migrate`, `backend/cmd/coolify-cli`)
-   PostgreSQL 16 (Primary persistence, `pgx/v5`, Laravel-compatible schema + `APP_KEY` encryption + `id = 0` sentinels)
-   Redis 7 (Cache coherence & distributed coordination under `coolify:go:v1:*` namespace)
-   Rust (`executor/crates/{protocol,runner}` -> `coolify-executor`: streaming SSH, process supervision, PTY terminal, whole-lifecycle timeout & host-boundary enforcement)

## DevOps & Infrastructure

-   Multi-stage standalone Docker (`docker/stack-migration/Dockerfile`, `alpine:3.21` non-root `uid=1000(coolify)`, zero PHP/Node/Svelte in production runtime)
-   Docker Compose (`docker/stack-migration/docker-compose.yml`, `docker/stack-migration/docker-compose.cutover.yml`)
-   Traefik / Caddy reverse proxy management
-   GitHub Actions (CI/CD)

## Languages

-   Go 1.26
-   Rust (2021 edition)
-   TypeScript / Astro / Vue 3.5
-   Shell/Bash scripts
