import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';

const rootDir = path.resolve(import.meta.dirname, '../..');
const webDir = path.join(rootDir, 'apps/web');

function listAllFiles(dir: string): string[] {
  const out: string[] = [];
  for (const ent of fs.readdirSync(dir, { withFileTypes: true })) {
    if (ent.name === 'node_modules' || ent.name === 'dist' || ent.name === '.astro') continue;
    const p = path.join(dir, ent.name);
    if (ent.isDirectory()) out.push(...listAllFiles(p));
    else out.push(p);
  }
  return out;
}

describe('Task 2 & 11: Astro + Vue (vue@^3.5.45) Web Shell & Frontend Contracts', () => {
  it('enforces Vue as sole UI framework and bans React/Svelte/VueRouter/Pinia', () => {
    const pkg = JSON.parse(fs.readFileSync(path.join(webDir, 'package.json'), 'utf8')) as {
      dependencies?: Record<string, string>;
      devDependencies?: Record<string, string>;
    };
    const allDeps = { ...pkg.dependencies, ...pkg.devDependencies };
    assert.ok(allDeps['@astrojs/vue'], 'expected @astrojs/vue');
    assert.ok(allDeps['vue'], 'expected vue');
    assert.match(allDeps['vue'], /^\^?3\./, 'installed official Vue major compatible with @astrojs/vue must be documented accurately (3.x)');
    assert.ok(allDeps['astro'], 'expected astro');

    for (const banned of ['react', 'react-dom', 'svelte', '@astrojs/react', '@astrojs/svelte', 'vue-router', 'pinia', 'nuxt']) {
      assert.equal(allDeps[banned], undefined, `banned framework/router dependency detected: ${banned}`);
    }
  });

  it('uses static Astro output, avoids raw HTML injection (innerHTML/v-html), and avoids em dashes / stale Vue 4 labels in UI copy', () => {
    const astroCfg = fs.readFileSync(path.join(webDir, 'astro.config.mjs'), 'utf8');
    assert.match(astroCfg, /output:\s*'static'/);
    assert.match(astroCfg, /vue\(\)/);

    const srcFiles = listAllFiles(path.join(webDir, 'src'));
    for (const f of srcFiles) {
      const txt = fs.readFileSync(f, 'utf8');
      assert.equal(/v-html|\.innerHTML\s*=/.test(txt), false, `unsafe raw HTML injection found in ${f}`);
      assert.equal(/Vue 4/.test(txt), false, `stale Vue 4 copy found in ${f}`);
      if (f.endsWith('.vue') || f.endsWith('.astro')) {
        assert.equal(/—/.test(txt), false, `em dash violates DESIGN.md copy rules in ${f}`);
      }
    }
  });

  it('includes all required domain Vue components and lifecycle/API helpers', () => {
    const required = [
      'src/layouts/AppLayout.astro',
      'src/pages/index.astro',
      'src/pages/login.astro',
      'src/pages/app.astro',
      'src/lib/api.ts',
      'src/lib/use-page-lifecycle.ts',
      'src/lib/realtime.ts',
      'src/features/dashboard/DashboardRoot.vue',
      'src/features/servers/ServerSummary.vue',
      'src/features/servers/ServerList.vue',
      'src/features/servers/ServerForm.vue',
      'src/features/servers/ServerSettings.vue',
      'src/features/deployments/DeployControl.vue',
      'src/features/deployments/DeploymentHistory.vue',
      'src/features/logs/LogViewer.vue',
      'src/features/terminal/WebTerminal.vue',
      'src/features/services/ServiceEditor.vue',
      'src/features/databases/DatabasePanel.vue',
      'src/features/backups/BackupManager.vue',
      'src/features/schedules/ScheduleList.vue',
      'src/features/projects/ProjectExplorer.vue',
      'src/features/teams/TeamAccess.vue',
      'src/features/proxy/ProxyConfig.vue',
      'src/features/notifications/NotificationSettings.vue',
      'src/features/canvas/CanvasGraph.vue',
      'src/features/auth/LoginPanel.vue',
      'src/features/auth/LogoutButton.vue',
      'src/features/transfer/ServerTransferPanel.vue',
    ];
    for (const rel of required) {
      assert.ok(fs.existsSync(path.join(webDir, rel)), `missing required frontend module: ${rel}`);
    }
  });

  it('verifies built dist bundle exists, includes /login page, and contains zero secrets or server env keys', () => {
    const distDir = path.join(webDir, 'dist');
    assert.ok(fs.existsSync(path.join(distDir, 'index.html')), 'missing dist/index.html');
    assert.ok(fs.existsSync(path.join(distDir, 'login/index.html')), 'missing dist/login/index.html');
    const loginHtml = fs.readFileSync(path.join(distDir, 'login/index.html'), 'utf8');
    assert.match(loginHtml, /Sign in to Goolify/);
    for (const f of listAllFiles(distDir)) {
      const txt = fs.readFileSync(f, 'utf8');
      assert.equal(/APP_KEY=|BEGIN OPENSSH PRIVATE KEY|coolify_test_pw/.test(txt), false, `secret material leaked into dist file ${f}`);
    }
  });

  it('verifies apiRequest injects CSRF on mutations and never stores session tokens in localStorage', () => {
    const apiTxt = fs.readFileSync(path.join(webDir, 'src/lib/api.ts'), 'utf8');
    assert.match(apiTxt, /X-CSRF-TOKEN/);
    assert.match(apiTxt, /credentials:\s*'same-origin'/);
    assert.equal(/localStorage/.test(apiTxt), false, 'session token must never be stored in localStorage');
  });
});
