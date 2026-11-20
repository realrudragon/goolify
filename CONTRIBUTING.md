# Contributing to Goolify

Thank you for your interest in Goolify! Goolify is an independent, community-driven re-architecture of
[Coolify](https://github.com/coollabsio/coolify) (by Andras Bacsai and the CoolLabs community) using a
Go 1.26 control plane, a Rust executor, and an Astro + Vue 3.5 + TypeScript web UI.

## Please respect both projects

- **Hobby & open-source project.** Goolify is a personal hobby project maintained in spare time and is not
  updated on a fixed schedule. You are completely welcome to **fork this repository** for your own needs
  or **submit pull requests** to help develop and improve Goolify.
- **Goolify issues belong here.** Do not open issues, discussions, or pull requests about Goolify in the
  upstream Coolify repository. The Coolify maintainers do not maintain Goolify.
- **Keep attribution intact.** Do not remove or change upstream copyright notices, the `LICENSE`, or the
  `NOTICE` file. When you add or change files, your contribution is licensed under the
  [Apache License 2.0](./LICENSE).

## Ways to help

- Report reproducible bugs (use the bug report template)
- Fix bugs and improve tests
- Improve documentation
- Improve compatibility with Coolify v4 data, APIs, and templates

## Development setup

Requirements: Docker, Go 1.26, Rust (stable), Node.js 24.

```bash
# Start the full stack (PostgreSQL 16, Redis 7, Go control plane & worker, Rust executor)
docker compose up -d --build        # or: ./scripts/dev start   |   .\coolify.ps1 start
```

Open `http://localhost:8000` and sign in with the local development account printed by the launcher.

## Tests (required for every change)

Every change must include tests. For bug fixes, write a failing test first.

```bash
# Go backend (real PostgreSQL 16 + Redis 7; see TEST_DATABASE_URL / TEST_REDIS_URL)
cd backend && go test -count=1 -p 1 ./...

# Rust executor
cd executor && cargo test --workspace

# Web UI
cd apps/web && npm run check && npm run build

# Contract tests
node --test tests/stack-migration/inventory.test.ts tests/stack-migration/web-shell.spec.ts
```

## Pull request guidelines

1. Keep pull requests focused on one change.
2. Describe **what** changed and **why** in your own words.
3. Tell us if AI tools helped you, so we can review appropriately.
4. Never add mock or hard-coded data to production code paths.
5. Authorize every server-side read and mutation, and scope queries to the resource's team
   (see [SECURITY.md](./SECURITY.md)).
6. Do not put exploit details or payloads in commits, PR descriptions, or code comments.

## Code of Conduct

By participating, you agree to follow our [Code of Conduct](./CODE_OF_CONDUCT.md).
