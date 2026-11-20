# Migration Frozen Baseline

- **Frozen Commit SHA:** `171eae00a76dff4f5a02c2b5130d7b2dfe219a7d`
- **Captured At:** 2026-11-19
- **Architecture Target:** Standalone `Astro + Vue + TypeScript + Go + Rust` (`vue@^3.5.45` with `@astrojs/vue@^7.1.6` — latest compatible official Vue major on npm, since Vue 4 is not yet published on npm; no Laravel dual-runtime coexistence; existing PostgreSQL schema, `APP_KEY` encryption, `id = 0` sentinels, logic, UX/UI `DESIGN.md`, and full feature parity preserved)
- **Inventory Counts (Observed from Source Files):**
  - Models (`app/Models`): 81
  - Policies (`app/Policies`): 29
  - Livewire Components (`app/Livewire`): 291
  - Jobs (`app/Jobs`): 84
  - Services (`app/Services`): 108
  - Actions (`app/Actions`): 110
  - API Controllers (`app/Http/Controllers/Api`): 54
  - Routes (`routes/api.php`, `routes/web.php`, `routes/channels.php`): 631
- **Legacy Build-Pack Policy (Frozen):**
  - New deployments support `dockerfile`, `railpack`, `dockerimage`, `dockercompose`, and `static`.
  - Existing `nixpacks` configurations stored in PostgreSQL remain explicitly readable/classified for compatibility decisions prior to cutover without silent deletion.
- **Instance Sentinels (`id = 0`):**
  - `teams.id = 0` (Root Team), `servers.id = 0` (Localhost Server), `instance_settings.id = 0`, `standalone_postgresqls.id = 0` (`coolify-db`), `standalone_dockers.id = 0`.
- **PHP Baseline Test Status:** `not_run` in local workspace (no isolated PostgreSQL test runner invoked during migration scaffolding; verified per-domain in Go/Node/Rust test suites).
