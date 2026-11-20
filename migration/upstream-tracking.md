# Upstream Tracking Policy

- **Frozen Upstream Checkpoint:** `171eae00a76dff4f5a02c2b5130d7b2dfe219a7d`
- **Policy:** Self-maintained standalone Go/Rust/Astro+Vue (`vue@^3.5.45`) codebase after migration; selectively monitor Coolify upstream releases and security advisories, evaluate impact, and port required behavior changes with attribution and regression tests.
- **License & Attribution:** Retain upstream license notices and record upstream commit SHAs for every ported behavior or security fix below.

| Date | Upstream Commit SHA | Security/Feature Summary | Go/Rust/Web Target Task | Status |
|---|---|---|---|---|
| 2026-11-19 | `171eae00a76dff4f5a02c2b5130d7b2dfe219a7d` | Initial frozen baseline checkpoint | Tasks 1–14 | frozen_baseline |
