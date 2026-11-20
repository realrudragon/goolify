import { describe, it, before, after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { execFileSync, spawn } from 'node:child_process';

const rootDir = path.resolve(import.meta.dirname, '../..');
const backendDir = path.join(rootDir, 'backend');
const distDir = path.join(rootDir, 'apps/web/dist');
const screenshotDir = path.join(rootDir, 'migration/receipts/screenshots');

const DB_DSN = process.env.TEST_DATABASE_URL || 'postgres://coolify:coolify_test_pw@127.0.0.1:15432/coolify_test?sslmode=disable';
const REDIS_URL = process.env.TEST_REDIS_URL || 'redis://127.0.0.1:16379/0';
const APP_KEY = 'base64:MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=';
const PORT = 18432;
const BASE_URL = `http://127.0.0.1:${PORT}`;

function findChromeBinary() {
  const candidates = [
    process.env.CHROME_PATH,
    path.join(process.env.LOCALAPPDATA || '', 'ms-playwright', 'chromium-1208', 'chrome-win64', 'chrome.exe'),
    path.join(process.env.LOCALAPPDATA || '', 'ms-playwright', 'chromium-1200', 'chrome-win64', 'chrome.exe'),
    'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
    'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
    '/usr/bin/chromium',
    '/usr/bin/google-chrome',
  ].filter(Boolean);
  for (const p of candidates) {
    if (fs.existsSync(p)) return p;
  }
  throw new Error('No Chromium/Chrome binary found on host');
}

class CDPClient {
  constructor(ws) {
    this.ws = ws;
    this.nextId = 1;
    this.pending = new Map();
    ws.addEventListener('message', (ev) => {
      const msg = JSON.parse(String(ev.data));
      if (msg.id && this.pending.has(msg.id)) {
        const { resolve, reject } = this.pending.get(msg.id);
        this.pending.delete(msg.id);
        if (msg.error) reject(new Error(`${msg.error.code}: ${msg.error.message}`));
        else resolve(msg.result);
      }
    });
  }

  send(method, params = {}) {
    const id = this.nextId++;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.ws.send(JSON.stringify({ id, method, params }));
    });
  }

  async eval(expression) {
    const res = await this.send('Runtime.evaluate', {
      expression,
      awaitPromise: true,
      returnByValue: true,
    });
    if (res.exceptionDetails) {
      throw new Error(`Browser eval exception: ${res.exceptionDetails.text || JSON.stringify(res.exceptionDetails)}`);
    }
    return res.result?.value;
  }

  async waitFor(expression, timeoutMs = 10000) {
    const deadline = Date.now() + timeoutMs;
    let lastVal;
    while (Date.now() < deadline) {
      try {
        lastVal = await this.eval(expression);
        if (lastVal) return lastVal;
      } catch {
        // Page may be mid-navigation
      }
      await new Promise((r) => setTimeout(r, 100));
    }
    const bodyText = await this.eval('document.body ? document.body.innerText : ""').catch(() => '');
    const href = await this.eval('window.location.href').catch(() => '');
    throw new Error(`waitFor timed out (${timeoutMs}ms) for: ${expression}\nurl=${href}\nlastVal=${JSON.stringify(lastVal)}\nbody=${bodyText.slice(0, 600)}`);
  }

  async screenshot(filename) {
    fs.mkdirSync(screenshotDir, { recursive: true });
    const res = await this.send('Page.captureScreenshot', { format: 'png' });
    const outPath = path.join(screenshotDir, filename);
    fs.writeFileSync(outPath, Buffer.from(res.data, 'base64'));
    return outPath;
  }

  close() {
    this.ws.close();
  }
}

async function waitForHTTP(url, timeoutMs = 15000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(url);
      if (res.ok) return;
    } catch {
      // Server still booting
    }
    await new Promise((r) => setTimeout(r, 150));
  }
  throw new Error(`Timed out waiting for ${url}`);
}

function psqlExec(sql) {
  execFileSync('docker', [
    'exec', 'coolify-mig-test-pg',
    'psql', '-U', 'coolify', '-d', 'coolify_test', '-v', 'ON_ERROR_STOP=1', '-c', sql,
  ], { stdio: 'pipe' });
}

describe('Real Headless Chromium Browser E2E against Production Go Control Plane', () => {
  let tmpDir;
  let binPath;
  let serverProc;
  let chromeProc;
  let cdp;

  before(async () => {
    tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'coolify-e2e-'));
    binPath = path.join(tmpDir, process.platform === 'win32' ? 'coolify-controlplane.exe' : 'coolify-controlplane');

    // 1. Reset isolated test database schema cleanly before booting real controlplane
    psqlExec('DROP SCHEMA public CASCADE; CREATE SCHEMA public;');

    // 2. Compile REAL production cmd/controlplane binary
    execFileSync('go', ['build', '-o', binPath, './cmd/controlplane'], {
      cwd: backendDir,
      stdio: 'pipe',
    });

    // 3. Start REAL production controlplane serving REAL built apps/web/dist
    serverProc = spawn(binPath, [
      '-addr', `127.0.0.1:${PORT}`,
      '-static-dir', distDir,
      '-bootstrap-schema=true',
    ], {
      env: {
        ...process.env,
        DATABASE_URL: DB_DSN,
        REDIS_URL: REDIS_URL,
        REDIS_PREFIX: 'coolify:go:v1:browser-e2e:',
        APP_KEY: APP_KEY,
        ROOT_USER_EMAIL: 'root@coolify.test',
        ROOT_USER_PASSWORD: 'RootSecret123!',
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    });

    let serverLogs = '';
    serverProc.stdout.on('data', (d) => { serverLogs += d.toString(); });
    serverProc.stderr.on('data', (d) => { serverLogs += d.toString(); });

    try {
      await waitForHTTP(`${BASE_URL}/api/v1/health`, 25000);
    } catch (err) {
      throw new Error(`${err.message}\ncontrolplane logs:\n${serverLogs}`);
    }

    // 4. Seed a second team (id=20) for root user + malicious XSS log line on localhost
    psqlExec(`
      INSERT INTO teams (id, name, personal_team) VALUES (20, 'Secondary Team', FALSE) ON CONFLICT (id) DO NOTHING;
      INSERT INTO team_user (team_id, user_id, role) VALUES (20, 0, 'owner') ON CONFLICT DO NOTHING;
      INSERT INTO go_operation_logs (resource_key, operation_id, stream, text)
      VALUES ('localhost', 'op-xss-browser', 'stdout', '<script>window.__coolify_xss=true</script>DEPLOY_SAFE_LINE');
    `);

    // 5. Launch real headless Chromium with remote debugging
    const chromeBin = findChromeBinary();
    const userDataDir = path.join(tmpDir, 'chrome-profile');
    const debugPort = 19222;
    chromeProc = spawn(chromeBin, [
      '--headless=new',
      '--disable-gpu',
      '--no-sandbox',
      '--window-size=1366,900',
      `--remote-debugging-port=${debugPort}`,
      `--user-data-dir=${userDataDir}`,
      'about:blank',
    ], { stdio: 'ignore' });

    let pageTarget = null;
    const cdpDeadline = Date.now() + 15000;
    while (Date.now() < cdpDeadline) {
      try {
        const res = await fetch(`http://127.0.0.1:${debugPort}/json/list`);
        if (res.ok) {
          const targets = await res.json();
          pageTarget = targets.find((t) => t.type === 'page' && t.webSocketDebuggerUrl) || null;
          if (pageTarget) break;
        }
      } catch {
        // Chromium still starting
      }
      await new Promise((r) => setTimeout(r, 150));
    }
    assert.ok(pageTarget?.webSocketDebuggerUrl, 'expected Chromium page CDP WebSocket URL');

    const ws = new WebSocket(pageTarget.webSocketDebuggerUrl);
    await new Promise((resolve, reject) => {
      ws.addEventListener('open', resolve, { once: true });
      ws.addEventListener('error', reject, { once: true });
    });
    cdp = new CDPClient(ws);
    await cdp.send('Page.enable');
    await cdp.send('Runtime.enable');
  });

  after(() => {
    try { cdp?.close(); } catch {}
    try { chromeProc?.kill('SIGKILL'); } catch {}
    try { serverProc?.kill('SIGKILL'); } catch {}
    try { fs.rmSync(tmpDir, { recursive: true, force: true }); } catch {}
  });

  it('redirects unauthenticated / to /login and renders Login UI (never Dashboard)', async () => {
    await cdp.send('Page.navigate', { url: `${BASE_URL}/` });
    await cdp.waitFor(`window.location.pathname === '/login' && document.body.innerText.includes('Sign in to Goolify')`);
    const text = await cdp.eval('document.body.innerText');
    assert.equal(text.includes('Managed Servers'), false, 'unauthenticated /login must NOT render dashboard widgets');
    await cdp.screenshot('01-login.png');
  });

  it('logs in via UI form, sets HttpOnly session + CSRF cookie, hydrates dashboard, and persists created Server + Project across full reload', async () => {
    await cdp.waitFor(`document.querySelector('form[data-hydrated="true"]') !== null`);
    await cdp.eval(`(() => {
      const email = document.querySelector('input[type="email"]');
      const pass = document.querySelector('input[type="password"]');
      email.value = 'root@coolify.test';
      email.dispatchEvent(new Event('input', { bubbles: true }));
      pass.value = 'RootSecret123!';
      pass.dispatchEvent(new Event('input', { bubbles: true }));
      document.querySelector('button[type="submit"]').click();
    })()`);

    await cdp.waitFor(`window.location.pathname === '/' && document.body.innerText.includes('Active Team ID: 0') && document.body.innerText.includes('localhost')`);

    const storageLen = await cdp.eval('window.localStorage.length');
    assert.equal(storageLen, 0, 'session tokens must NEVER be stored in localStorage');

    const cookies = await cdp.eval('document.cookie');
    assert.match(cookies, /coolify_csrf=/, 'expected readable coolify_csrf cookie');
    assert.equal(cookies.includes('coolify_session='), false, 'coolify_session cookie must be HttpOnly');

    // Create a Project through the UI (uses real CSRF header -> PostgreSQL)
    await cdp.eval(`(() => {
      const input = document.querySelector('input[aria-label="New project name"]');
      input.value = 'Browser E2E Project';
      input.dispatchEvent(new Event('input', { bubbles: true }));
      input.closest('form').querySelector('button[type="submit"]').click();
    })()`);
    await cdp.waitFor(`document.body.innerText.includes('Browser E2E Project')`);

    // Create a Server through the UI (uses real CSRF header -> PostgreSQL)
    await cdp.eval(`(() => {
      const name = document.querySelector('input[aria-label="Server Name"]');
      const ip = document.querySelector('input[aria-label="IP / Hostname"]');
      name.value = 'edge-node-1';
      name.dispatchEvent(new Event('input', { bubbles: true }));
      ip.value = '10.20.30.40';
      ip.dispatchEvent(new Event('input', { bubbles: true }));
      name.closest('form').querySelector('button[type="submit"]').click();
    })()`);
    await cdp.waitFor(`document.body.innerText.includes('edge-node-1')`);
    await cdp.screenshot('02-dashboard.png');

    // Full browser reload MUST keep both records from PostgreSQL
    await cdp.send('Page.reload', { ignoreCache: true });
    await cdp.waitFor(`document.body.innerText.includes('Browser E2E Project') && document.body.innerText.includes('edge-node-1')`);
  });

  it('switches teams via POST /app/auth/switch-team and clears stale Team 0 data with zero cross-team leakage', async () => {
    await cdp.eval(`document.querySelector('[data-testid="switch-team-20"]').click()`);
    await cdp.waitFor(`document.querySelector('[data-testid="active-team-id"]')?.textContent?.includes('20') && document.body.innerText.includes('No servers in this team.')`);
    const bodyTeam20 = await cdp.eval('document.body.innerText');
    assert.equal(bodyTeam20.includes('edge-node-1'), false, 'Team 0 server leaked after switching to Team 20');
    assert.equal(bodyTeam20.includes('Browser E2E Project'), false, 'Team 0 project leaked after switching to Team 20');

    // Switch back to Team 0
    await cdp.eval(`document.querySelector('[data-testid="switch-team-0"]').click()`);
    await cdp.waitFor(`document.querySelector('[data-testid="active-team-id"]')?.textContent?.includes('0') && document.body.innerText.includes('edge-node-1')`);
  });

  it('streams SSE logs XSS-safely and executes live commands over WebSocket terminal', async () => {
    await cdp.eval(`document.querySelector('[data-testid="tab-operations"]').click()`);

    // Ensure resource UUID is set to 'localhost' to replay seeded XSS log line
    await cdp.waitFor(`Boolean(document.querySelector('[data-testid="log-resource-input"]'))`);
    await cdp.eval(`(() => {
      const inp = document.querySelector('[data-testid="log-resource-input"]');
      if (inp.value !== 'localhost') {
        inp.value = 'localhost';
        inp.dispatchEvent(new Event('input', { bubbles: true }));
      }
    })()`);

    await cdp.waitFor(`document.body.innerText.includes('DEPLOY_SAFE_LINE')`);
    const xssFlag = await cdp.eval('window.__coolify_xss');
    assert.equal(xssFlag, undefined, 'XSS payload executed in LogViewer!');
    const logText = await cdp.eval(`Array.from(document.querySelectorAll('[data-testid="log-line"]')).map(el => el.textContent).join('\\n')`);
    assert.ok(logText.includes('<script>window.__coolify_xss=true</script>DEPLOY_SAFE_LINE'), `expected literal escaped script tag in textContent, got: ${logText}`);

    // Wait for WebSocket terminal connected & run live command
    await cdp.waitFor(`document.querySelector('[data-testid="terminal-status"]')?.textContent?.includes('Connected')`);
    await cdp.eval(`(() => {
      const inp = document.querySelector('input[aria-label="Terminal command"]');
      inp.value = 'echo BROWSER_WS_TERM_OK';
      inp.dispatchEvent(new Event('input', { bubbles: true }));
      inp.closest('form').querySelector('button[type="submit"]').click();
    })()`);
    await cdp.waitFor(`document.querySelector('[data-testid="terminal-output"]')?.textContent?.includes('BROWSER_WS_TERM_OK')`);
    await cdp.screenshot('03-logs-terminal.png');
  });

  it('creates and revokes a Sanctum API token in Settings & Transfer UI', async () => {
    await cdp.eval(`document.querySelector('[data-testid="tab-settings"]').click()`);
    await cdp.waitFor(`Boolean(document.querySelector('input[aria-label="Token label"]'))`);
    await cdp.eval(`(() => {
      const inp = document.querySelector('input[aria-label="Token label"]');
      inp.value = 'Browser CI Token';
      inp.dispatchEvent(new Event('input', { bubbles: true }));
      inp.closest('form').querySelector('button[type="submit"]').click();
    })()`);
    await cdp.waitFor(`document.body.innerText.includes('API token created') && document.body.innerText.includes('Browser CI Token')`);
    await cdp.eval(`(() => {
      const btns = Array.from(document.querySelectorAll('button'));
      const rev = btns.find((b) => b.textContent.trim() === 'Revoke');
      if (!rev) throw new Error('Revoke button not found');
      rev.click();
    })()`);
    await cdp.waitFor(`document.body.innerText.includes('API token revoked')`);
  });

  it('exercises Sources & Cloud and Security & Scans interactive Vue 3.5 panels', async () => {
    await cdp.eval(`document.querySelector('[data-testid="tab-sources"]').click()`);
    await cdp.waitFor(`Boolean(document.querySelector('[data-testid="sources-cloud-panel"]'))`);
    await cdp.eval(`(() => {
      const inp = document.querySelector('[data-testid="sources-cloud-panel"] input[placeholder="New GitHub App name"]');
      inp.value = 'Browser GH Source';
      inp.dispatchEvent(new Event('input', { bubbles: true }));
      const btn = Array.from(document.querySelectorAll('[data-testid="sources-cloud-panel"] button')).find(b => b.textContent.includes('Add GitHub App'));
      btn.click();
    })()`);
    await cdp.waitFor(`document.body.innerText.includes('GitHub App created.') && document.body.innerText.includes('Browser GH Source')`);

    await cdp.eval(`document.querySelector('[data-testid="tab-security"]').click()`);
    await cdp.waitFor(`Boolean(document.querySelector('[data-testid="security-vuln-panel"]'))`);
    await cdp.eval(`(() => {
      const btn = Array.from(document.querySelectorAll('[data-testid="security-vuln-panel"] button')).find(b => b.textContent.includes('Image Scanning:'));
      btn.click();
    })()`);
    await cdp.waitFor(`document.body.innerText.includes('Image scanning enabled.')`);
    await cdp.screenshot('04-sources-security.png');
  });

  it('exercises real-DB Projects/Apps/Services, Databases/Backups/Schedules, ServerOps, Canvas persistence & Governance UI', async () => {
    // 1. Canvas real position persistence in Overview
    await cdp.eval(`document.querySelector('[data-testid="tab-overview"]').click()`);
    await cdp.waitFor(`Boolean(document.querySelector('[data-testid="canvas-graph"]')) && !document.querySelector('[data-testid="canvas-graph"]').innerText.includes('(0 live nodes)')`);
    await cdp.eval(`(() => {
      const btn = Array.from(document.querySelectorAll('[data-testid="canvas-graph"] button')).find(b => b.textContent.includes('Save Layout'));
      btn.click();
    })()`);
    await cdp.waitFor(`document.body.innerText.includes('Canvas positions saved to PostgreSQL')`);

    // 2. Projects & Deploy: create real Project -> real Application + Compose Service
    await cdp.eval(`document.querySelector('[data-testid="tab-projects"]').click()`);
    await cdp.waitFor(`Boolean(document.querySelector('[data-testid="resources-manager"]'))`);
    await cdp.eval(`(() => {
      const inp = document.querySelector('input[aria-label="New project name"]');
      inp.value = 'Browser E2E Project';
      inp.dispatchEvent(new Event('input', { bubbles: true }));
      inp.closest('form').querySelector('button[type="submit"]').click();
    })()`);
    await cdp.waitFor(`document.body.innerText.includes('Project created: Browser E2E Project')`);
    // Refresh ResourcesManager so new project appears in select
    await cdp.eval(`(() => {
      const btn = Array.from(document.querySelectorAll('[data-testid="resources-manager"] button')).find(b => b.textContent.trim() === 'Refresh');
      btn.click();
    })()`);
    await cdp.waitFor(`document.querySelector('[data-testid="resources-manager"] select[aria-label="Environment"]')?.options?.length > 0`);
    await cdp.eval(`(() => {
      const inp = document.querySelector('[data-testid="new-app-name"]');
      inp.value = 'browser-live-app';
      inp.dispatchEvent(new Event('input', { bubbles: true }));
      document.querySelector('[data-testid="create-app-btn"]').click();
    })()`);
    await cdp.waitFor(`document.body.innerText.includes('Application created: browser-live-app')`);

    // 3. Databases & Backups: create real PostgreSQL DB + real Backup Schedule shown in ScheduleList (zero hardcoded rows!)
    await cdp.eval(`document.querySelector('[data-testid="tab-databases"]').click()`);
    await cdp.waitFor(`Boolean(document.querySelector('[data-testid="database-panel"]'))`);
    await cdp.waitFor(`document.querySelector('[data-testid="database-panel"] select[aria-label="Target project"]')?.options?.length > 0`);
    await cdp.eval(`(() => {
      const inp = document.querySelector('[data-testid="db-name-input"]');
      inp.value = 'browser-live-pg';
      inp.dispatchEvent(new Event('input', { bubbles: true }));
      document.querySelector('[data-testid="db-create-btn"]').click();
    })()`);
    await cdp.waitFor(`document.body.innerText.includes('Created postgresql database: browser-live-pg')`);
    await cdp.eval(`document.querySelector('[data-testid="create-backup-schedule-btn"]').click()`);
    await cdp.waitFor(`document.body.innerText.includes('Backup schedule saved:') && document.querySelector('[data-testid="schedule-list"]').innerText.includes('DB Backup')`);

    // 4. Servers tab: ServerOpsPanel (Swarm + Host Scan)
    await cdp.eval(`document.querySelector('[data-testid="tab-servers"]').click()`);
    await cdp.waitFor(`Boolean(document.querySelector('[data-testid="server-ops-panel"]'))`);
    await cdp.eval(`(() => {
      const btn = Array.from(document.querySelectorAll('[data-testid="server-ops-panel"] button')).find(b => b.textContent.includes('Record Host Scan'));
      btn.click();
    })()`);
    await cdp.waitFor(`document.body.innerText.includes('Host security scan recorded')`);

    // 5. Settings tab: GovernanceAndAuthPanel (Tag + MCP toggle)
    await cdp.eval(`document.querySelector('[data-testid="tab-settings"]').click()`);
    await cdp.waitFor(`Boolean(document.querySelector('[data-testid="governance-auth-panel"]'))`);
    await cdp.eval(`(() => {
      const inp = document.querySelector('[data-testid="governance-auth-panel"] input[placeholder="New tag name"]');
      inp.value = 'e2e-live-tag';
      inp.dispatchEvent(new Event('input', { bubbles: true }));
      inp.closest('form').querySelector('button[type="submit"]').click();
    })()`);
    await cdp.waitFor(`document.body.innerText.includes('Tag saved')`);
    await cdp.screenshot('05-full-workbench.png');
  });

  it('serves 200 HTML shell across Coolify baseline UI routes while keeping unknown API routes JSON 404', async () => {
    const uiRoutes = [
      '/servers', '/servers/canvas', '/server/localhost/proxy', '/destinations',
      '/projects', '/projects/canvas', '/databases', '/services', '/deployments',
      '/sources', '/security', '/security/api-tokens', '/security/integration-tokens',
      '/vulnerabilities', '/settings', '/settings/oauth', '/settings/email',
      '/notifications/discord', '/storages', '/shared-variables/team', '/tags',
      '/team/members', '/team/audit-log', '/profile', '/subscription', '/admin', '/onboarding', '/terminal',
    ];
    for (const r of uiRoutes) {
      const status = await cdp.eval(`fetch(${JSON.stringify(r)}, { credentials: 'same-origin' }).then(res => res.status)`);
      assert.equal(status, 200, `expected 200 HTML shell for UI route ${r}, got ${status}`);
    }
    const api404 = await cdp.eval(`fetch('/api/v1/does-not-exist', { credentials: 'same-origin' }).then(async res => ({ s: res.status, ct: res.headers.get('content-type') || '' }))`);
    assert.equal(api404.s, 404);
    assert.ok(api404.ct.includes('application/json'), `expected JSON 404 on unknown /api/v1, got ${api404.ct}`);
  });

  it('logs out via Sign out button, revokes PostgreSQL session, and returns 401 on subsequent /app/auth/me', async () => {
    await cdp.eval(`(() => {
      const btns = Array.from(document.querySelectorAll('button'));
      const signOut = btns.find((b) => b.textContent.trim() === 'Sign out');
      if (!signOut) throw new Error('Sign out button not found');
      signOut.click();
    })()`);
    await cdp.waitFor(`window.location.pathname === '/login' && document.body.innerText.includes('Sign in to Goolify')`);

    const meStatus = await cdp.eval(`fetch('/app/auth/me', { credentials: 'same-origin' }).then(r => r.status)`);
    assert.equal(meStatus, 401, 'expected 401 from /app/auth/me after logout');
  });
});
