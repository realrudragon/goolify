<p align="center">
  <img src="apps/web/public/goolify-logo.svg" alt="Goolify logo" width="96" height="96" />
</p>

# Goolify

> **Forked from [coollabsio/coolify](https://github.com/coollabsio/coolify).** Goolify is an independent, community re-architecture of Coolify. All credit for the original product, design, and service catalog goes to **[Andras Bacsai](https://github.com/andrasbacsai)** and the **Coolify / CoolLabs community**. ❤️

**Goolify** is a standalone, native-compiled, self-hostable PaaS created and maintained by **Worachote Pansanit**. Re-engineered from the ground up with a **Go 1.26** control plane & worker, a **Rust** SSH/process execution engine, and an **Astro + Vue 3.5 + TypeScript** frontend, Goolify delivers full Coolify v4 compatibility inside a **47.45 MB** Alpine container with zero PHP or Node.js runtime overhead.

---

## Credits, Origin & Attribution to Coolify

**Goolify** is directly inspired by and derived from **[Coolify](https://github.com/coollabsio/coolify)**, originally created by **[Andras Bacsai (@andrasbacsai)](https://github.com/andrasbacsai)** and the CoolLabs open-source community under the **Apache License, Version 2.0**.

- **Original Project:** [github.com/coollabsio/coolify](https://github.com/coollabsio/coolify)
- **Original Author & Copyright:** Copyright 2025 Andras Bacsai & Contributors ([LICENSE](./LICENSE))
- **Goolify Author & Copyright:** Copyright 2026 **Worachote Pansanit** ([NOTICE](./NOTICE))
- **Upstream Assets Preserved:** Service template catalog (`templates/`), service SVGs (`svgs/`), Compose parser reference fixtures (`tests/Fixtures/parser/`), PostgreSQL v4 schema and `id = 0` instance sentinel conventions, Laravel `APP_KEY` encryption format, and product design/security specifications (`DESIGN.md`, `SECURITY.md`).

> **Disclaimer (Apache License 2.0, Section 6):** *Goolify is an independent open-source project authored by Worachote Pansanit and is not affiliated with, endorsed by, or officially supported by CoolLabs or Andras Bacsai. "Coolify" and "CoolLabs" remain trademarks of their respective owners.*

---

## Architecture & Measured Benchmarks

Goolify replaces the PHP/Laravel/Livewire/Horizon/Reverb/Node stack with compiled Go and Rust binaries paired with a static Astro + Vue 3.5 web UI:

| Component / Metric | Original Coolify v4 | Goolify (Standalone Go + Rust + Astro/Vue) |
| :--- | :--- | :--- |
| **Control Plane & API** | PHP 8.x / Laravel / Octane | **Go 1.26** (`coolify-controlplane`) |
| **Background Worker & Scheduler** | Laravel Horizon + PHP Cron | **Go 1.26** (`coolify-worker`, `FOR UPDATE SKIP LOCKED`) |
| **SSH / Process / PTY Executor** | PHP `ssh` + Node Terminal server | **Rust** (`coolify-executor`, NDJSON streaming & lifecycle timeouts) |
| **Web Frontend** | Laravel Blade / Livewire / Alpine | **Astro + Vue 3.5 + TypeScript + Tailwind CSS v4** (static bundle) |
| **Database & Cache** | PostgreSQL + Redis | **PostgreSQL 16 + Redis 7** (`coolify:go:v1:*` coherent cache) |
| **Production Image Size** | Multi-runtime PHP + Node image | **47.45 MB** (`alpine:3.21`, `uid=1000(coolify)`, zero PHP/Node) |
| **Control Plane Memory (Idle / Active)** | — | **62.17 MB** idle / **67.09 MB** active RSS |
| **Worker Memory** | — | **61.77 MB** RSS |
| **API Latency (100 samples)** | — | **p50 = 3.89 ms** / **p95 = 12.91 ms** / **p99 = 20.44 ms** |
| **SSE Log Streaming** | — | **5,481.1 lines/sec** |
| **WebSocket Terminal Round-Trip** | — | **252.75 ms** |

*(Metrics recorded in `migration/receipts/benchmark-report.json` against PostgreSQL 16 + Redis 7.)*

---

## 1-Command Quick Start

### Ubuntu / Linux Server (One-Line Installer)

Install Docker, clone Goolify to `/opt/goolify`, generate a secure `.env` (`APP_KEY`, `DB_PASSWORD`, admin credentials), build & start the stack, and install the `goolify` CLI helper in one command:

```bash
curl -fsSL https://raw.githubusercontent.com/realrudragon/goolify/main/scripts/install.sh | sudo bash
```

Review [scripts/install.sh](./scripts/install.sh) before running. After installation, manage the instance anywhere with `goolify status`, `goolify logs`, `goolify restart`, or `goolify upgrade`.

### Local Development (All OS)

Start the complete Goolify stack (PostgreSQL 16, Redis 7, Go control plane, Go worker, and Rust executor) with zero manual environment setup:

### Docker Compose (All OS)
```bash
docker compose up -d
```

### Windows (PowerShell)
```powershell
.\coolify.ps1
```

### Linux / macOS / Git Bash
```bash
./scripts/dev start
```

On first boot, `coolify-controlplane` (`-bootstrap-schema=true`) automatically creates all PostgreSQL tables, seeds the `id = 0` instance sentinels, and provisions the default root administrator:

- **Web UI:** `http://localhost:8000`
- **Default Email:** `admin@coolify.local`
- **Default Password:** `CoolifyAdmin123!`

> [!WARNING]
> **The defaults above are public and for local testing only.** Before you expose Goolify to a network, set your own `APP_KEY`, `APP_URL`, `APP_PORT`, `DB_PASSWORD`, `ROOT_USER_EMAIL`, and `ROOT_USER_PASSWORD` in your environment or a `.env` file. Generate an `APP_KEY` with `echo "base64:$(openssl rand -base64 32)"`. If you migrate an existing Coolify v4 database, reuse that instance's original `APP_KEY` so encrypted secrets stay readable.

### Everyday Management Commands

| Action | Windows PowerShell | Linux / macOS / Bash | Docker Compose |
| :--- | :--- | :--- | :--- |
| Start / rebuild | `.\coolify.ps1 start` | `./scripts/dev start` | `docker compose up -d --build` |
| Service status | `.\coolify.ps1 status` | `./scripts/dev status` | `docker compose ps` |
| Follow logs | `.\coolify.ps1 logs` | `./scripts/dev logs` | `docker compose logs -f --tail=100` |
| Stop (keep data) | `.\coolify.ps1 stop` | `./scripts/dev stop` | `docker compose stop` |
| Remove containers | `.\coolify.ps1 down` | `./scripts/dev down` | `docker compose down --remove-orphans` |
| Run CLI commands | `.\coolify.ps1 cli schedule:diagnostics` | `./scripts/dev cli schedule:diagnostics` | `docker compose exec controlplane /usr/local/bin/coolify-cli ...` |

---

## 100% Coolify Compatibility & Parity

Goolify maintains wire, cryptographic, database, and behavioral parity with Coolify v4 (tracked across `migration/features.csv`, `migration/routes.csv`, and `migration/data-coverage.csv`):

- **Deployments & Build Packs:** `dockerfile`, `railpack`, `dockercompose`, `dockerimage`, and `static` across public repos, GitHub Apps, GitLab Apps, Gitea Apps, and SSH deploy keys — supporting rolling updates, previews, webhooks, rollbacks, and resource/environment cloning.
- **9 Standalone Databases:** PostgreSQL, MySQL, MariaDB, MongoDB, Redis, ClickHouse, KeyDB, Dragonfly, and SQLite with scheduled S3 backups, TLS/proxy toggles, and full cloning.
- **380+ One-Click Services:** Loads the full upstream catalog (`templates/service-templates-latest.json` and `templates/compose/*.yaml`) directly in Go, syncing `service_applications` and `service_databases`.
- **Encrypted Database Migration (`APP_KEY`):** Supports bidirectional AES-256-CBC + HMAC-SHA256 Laravel cipher payloads, `id = 0` sentinels, Sanctum-compatible SHA-256 API tokens (`read`, `write`, `deploy`, `root`), and `/mcp` access.
- **Multi-Tenant Teams, RBAC & Server Transfer:** Team-scoped authorization (`MEMBER` < `ADMIN` < `OWNER`), access grants, 2FA/Passkeys/OAuth, real-time SSE events, WebSocket PTY terminal, and full Server Transfer bundles.

---

## Development & Test Commands

```bash
# Go backend tests (real PostgreSQL 16 + Redis 7)
cd backend && go test -count=1 -p 1 ./...

# Rust executor tests
cd executor && cargo test --workspace

# Astro + Vue 3.5 frontend check & build
cd apps/web && npm run check && npm run build

# Contract, Chromium E2E & packaging suites
node --test tests/stack-migration/inventory.test.ts tests/stack-migration/web-shell.spec.ts
node --test tests/stack-migration/browser-e2e.spec.mjs
node --test tests/stack-migration/packaging-benchmark.spec.ts
```

---

## Where to Report Issues

- **Goolify bugs and questions:** please use this repository's Issues and Discussions.
- **Security issues in Goolify:** report privately via the **Security** tab (see [SECURITY.md](./SECURITY.md)).
- **Please do not report Goolify problems to the Coolify team.** They do not maintain Goolify. If a problem also exists in upstream Coolify, report it to [coollabsio/coolify](https://github.com/coollabsio/coolify) following their guidelines.

---

## ❤️ Support the Original Project

Goolify would not exist without Coolify. The product design, the data model, and the 380+ one-click service templates come from years of work by Andras Bacsai and hundreds of Coolify contributors. If Goolify is useful to you, please also:

- ⭐ Star [coollabsio/coolify](https://github.com/coollabsio/coolify)
- 💜 Sponsor the Coolify maintainers: [coolify.io/sponsorships](https://coolify.io/sponsorships)
- 🧩 Contribute new service templates upstream, so the whole community benefits

---

## License

Licensed under the **Apache License, Version 2.0**. See [LICENSE](./LICENSE) and [NOTICE](./NOTICE) for copyright and upstream attribution details.

- **Goolify Author:** Worachote Pansanit
- **Based on Coolify:** Created by [Andras Bacsai](https://github.com/andrasbacsai) and the [CoolLabs](https://github.com/coollabsio/coolify) community
