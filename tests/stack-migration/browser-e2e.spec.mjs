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
      throw new Error(`Browser eval exception: ${res.exceptionDetails.exception?.description || res.exceptionDetails.text || JSON.stringify(res.exceptionDetails)}`);
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
  const database = new URL(DB_DSN);
  execFileSync('docker', [
    'exec', process.env.TEST_PG_CONTAINER || 'coolify-mig-test-pg',
    'psql', '-U', decodeURIComponent(database.username), '-d', decodeURIComponent(database.pathname.slice(1)), '-v', 'ON_ERROR_STOP=1', '-c', sql,
  ], { stdio: 'pipe' });
}

describe('Real Headless Chromium Browser E2E against Production Go Control Plane', () => {
  let tmpDir;
  let binPath;
  let serverProc;
  let chromeProc;
  let cdp;
  let applicationDeepLink;
  async function navigateWorkspace(section) {
    const routes = { overview: '/', servers: '/servers', projects: '/projects', operations: '/terminal', databases: '/databases', sources: '/sources', security: '/vulnerabilities', settings: '/settings' };
    await cdp.send('Page.navigate', { url: BASE_URL + routes[section] });
    await cdp.waitFor(`document.querySelector('[data-workspace="${section}"][data-hydrated="true"]') !== null`);
  }
  async function navigatePage(route, page) {
    await cdp.send('Page.navigate', { url: BASE_URL + route });
    await cdp.waitFor(`document.querySelector('[data-page="${page}"][data-hydrated="true"]') !== null`);
  }
  async function clickText(text, scope = 'main') {
    await cdp.waitFor(`Array.from(document.querySelectorAll(${JSON.stringify(scope + ' button')})).some(button => button.textContent.trim() === ${JSON.stringify(text)} && !button.disabled)`);
    await cdp.eval(`Array.from(document.querySelectorAll(${JSON.stringify(scope + ' button')})).find(button => button.textContent.trim() === ${JSON.stringify(text)} && !button.disabled).click()`);
  }
  async function fillLabel(label, value, scope = 'main') {
    await cdp.waitFor(`Array.from(document.querySelectorAll(${JSON.stringify(scope)} + ' label')).some(el => el.textContent.trim().startsWith(${JSON.stringify(label)}) && el.querySelector('input,textarea,select'))`);
    await cdp.eval(`(() => { const field = Array.from(document.querySelectorAll(${JSON.stringify(scope + ' label')})).find(label => label.firstChild?.textContent?.trim() === ${JSON.stringify(label)}); if (!field) throw new Error('Missing label: ' + ${JSON.stringify(label)}); const input = field.querySelector('input, textarea, select'); input.value = ${JSON.stringify(value)}; input.dispatchEvent(new Event(input.tagName === 'SELECT' ? 'change' : 'input', { bubbles: true })); })()`);
  }

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
    await cdp.send('Page.addScriptToEvaluateOnNewDocument', { source: `
      window.__realtimeURLs = [];
      window.__apiURLs = [];
      const originalFetch = window.fetch;
      window.fetch = function(input, config) {
        window.__apiURLs.push(input instanceof Request ? input.url : String(input));
        return originalFetch.call(this, input, config);
      };
      const OriginalEventSource = window.EventSource;
      window.EventSource = class extends OriginalEventSource {
        constructor(url, config) {
          super(url, config);
          window.__realtimeURLs.push(String(url));
        }
      };
    ` });
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
    const loginLayout = await cdp.eval(`({ width: document.querySelector('form').getBoundingClientRect().width, emailLabel: document.querySelector('label[for="login-email"]')?.textContent?.trim(), passwordLabel: document.querySelector('label[for="login-password"]')?.textContent?.trim() })`);
    assert.equal(loginLayout.emailLabel, 'Email');
    assert.equal(loginLayout.passwordLabel, 'Password');
    assert.equal(loginLayout.width, 432);
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

    await cdp.waitFor(`window.location.pathname === '/' && document.querySelector('[data-testid="active-team-id"]')?.dataset.teamId === '0' && document.body.innerText.includes('localhost')`);

    assert.equal(await cdp.eval(`Boolean(document.querySelector('table[aria-label="Projects"]')) && Boolean(document.querySelector('table[aria-label="Servers"]'))`), true, 'dashboard must use the original Projects and Servers tables');
    assert.equal(await cdp.eval(`Boolean(document.querySelector('[role="tablist"][aria-label="Workspace sections"]'))`), false, 'dashboard must not render the combined workbench tabs');
    assert.equal(await cdp.eval(`Boolean(document.querySelector('input[aria-label="Server Name"]'))`), false, 'server creation belongs behind its own action');

    const storageLen = await cdp.eval('window.localStorage.length');
    assert.equal(storageLen, 0, 'session tokens must NEVER be stored in localStorage');

    const cookies = await cdp.eval('document.cookie');
    assert.match(cookies, /coolify_csrf=/, 'expected readable coolify_csrf cookie');
    assert.equal(cookies.includes('coolify_session='), false, 'coolify_session cookie must be HttpOnly');

    // Create from the dashboard's original + action, rather than an inline form.
    await cdp.eval(`document.querySelector('[data-testid="new-project"]').click()`);
    await cdp.waitFor(`document.querySelector('dialog[aria-labelledby="new-project-title"]').open`);
    // Create a Project through the UI (uses real CSRF header -> PostgreSQL)
    await cdp.eval(`(() => {
      const input = document.querySelector('input[aria-label="New project name"]');
      input.value = 'Browser E2E Project';
      input.dispatchEvent(new Event('input', { bubbles: true }));
      input.closest('form').querySelector('button[type="submit"]').click();
    })()`);
    await cdp.waitFor(`document.body.innerText.includes('Browser E2E Project')`);

    await cdp.send('Page.navigate', { url: `${BASE_URL}/servers/new` });
    await cdp.waitFor(`document.querySelector('input[aria-label="Server Name"]') !== null && document.querySelector('[data-workspace="servers"][data-hydrated="true"]') !== null`);
    await cdp.waitFor(`Boolean(document.querySelector('select[aria-label="Private Key UUID"]')?.value)`);
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
    await cdp.waitFor(`document.body.innerText.includes('Server registered')`);
    await navigateWorkspace('overview');
    await cdp.waitFor(`document.body.innerText.includes('Browser E2E Project') && document.body.innerText.includes('edge-node-1')`);
    await cdp.screenshot('02-dashboard.png');

    // Full browser reload MUST keep both records from PostgreSQL
    await cdp.send('Page.reload', { ignoreCache: true });
    await cdp.waitFor(`document.body.innerText.includes('Browser E2E Project') && document.body.innerText.includes('edge-node-1')`);
  });

  it('switches teams via POST /app/auth/switch-team and clears stale Team 0 data with zero cross-team leakage', async () => {
    await cdp.eval(`document.querySelector('[data-testid="switch-team-20"]').click()`);
    await cdp.waitFor(`document.querySelector('[data-testid="active-team-id"]')?.dataset.teamId === '20' && document.body.innerText.includes('No servers in this team.')`);
    const bodyTeam20 = await cdp.eval('document.body.innerText');
    assert.equal(bodyTeam20.includes('edge-node-1'), false, 'Team 0 server leaked after switching to Team 20');
    assert.equal(bodyTeam20.includes('Browser E2E Project'), false, 'Team 0 project leaked after switching to Team 20');
    await cdp.waitFor(`window.__realtimeURLs.filter(url => url.startsWith('/realtime/events?')).at(-1) === '/realtime/events?channel=team.20'`);

    // Switch back to Team 0
    await cdp.eval(`document.querySelector('[data-testid="switch-team-0"]').click()`);
    await cdp.waitFor(`document.querySelector('[data-testid="active-team-id"]')?.dataset.teamId === '0' && document.body.innerText.includes('edge-node-1')`);
    await cdp.waitFor(`window.__realtimeURLs.filter(url => url.startsWith('/realtime/events?')).at(-1) === '/realtime/events?channel=team.0'`);
  });

  it('streams SSE logs XSS-safely and executes live commands over WebSocket terminal', async () => {
    await navigateWorkspace('operations');

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

  it('renders separate original server and destination screens and persists destination changes', async () => {
    await navigatePage('/servers', 'servers');
    await cdp.waitFor(`document.querySelector('[data-testid="servers-page"] table')?.innerText.includes('edge-node-1')`);
    assert.equal(await cdp.eval(`document.querySelector('main h1').textContent`), 'Servers');
    assert.equal(await cdp.eval(`Boolean(document.querySelector('main input[aria-label="Server Name"]'))`), false);
    assert.equal(await cdp.eval(`document.body.innerText.includes('Server Operations')`), false);
    await cdp.screenshot('09-original-servers.png');
    await navigatePage('/destinations', 'destinations');
    await cdp.waitFor(`document.querySelector('[data-testid="destinations-page"] table')?.innerText.includes('Standalone Docker')`);
    assert.equal(await cdp.eval(`document.querySelector('main h1').textContent`), 'Destinations');
    assert.equal(await cdp.eval(`document.body.innerText.includes('Managed Servers') || document.body.innerText.includes('Save Server')`), false);
    await cdp.screenshot('10-original-destinations.png');
    await clickText('New destination');
    await fillLabel('Name', 'browser-network', 'dialog[open]');
    await fillLabel('Docker network', 'browser-network', 'dialog[open]');
    await clickText('Create', 'dialog[open]');
    await cdp.waitFor(`location.pathname.startsWith('/destination/') && document.querySelector('[data-settings-section-title="General"]') !== null`);
    const destinationPath = await cdp.eval('location.pathname');
    await fillLabel('Name', 'browser-network-renamed');
    await clickText('Save');
    await cdp.waitFor(`document.querySelector('main h1')?.textContent === 'browser-network-renamed'`);
    await cdp.send('Page.reload');
    await cdp.waitFor(`document.querySelector('main h1')?.textContent === 'browser-network-renamed'`);
    await navigatePage(destinationPath + '/danger', 'destination');
    await clickText('Delete destination');
    await fillLabel('Destination Name', 'browser-network-renamed', 'dialog[open]');
    await clickText('Permanently Delete', 'dialog[open]');
    await cdp.waitFor(`location.pathname === '/destinations' && document.querySelector('[data-testid="destinations-page"]') !== null`);
    await cdp.waitFor(`!document.body.innerText.includes('browser-network-renamed')`);
  });

  it('creates and revokes a Sanctum API token in Settings & Transfer UI', async () => {
    await navigatePage('/security/api-tokens', 'api-tokens');
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
    await navigateWorkspace('sources');
    await cdp.waitFor(`Boolean(document.querySelector('[data-testid="sources-page"]'))`);
    assert.equal(await cdp.eval(`document.body.innerText.includes('Cloud Provider Tokens')`), false);
    await clickText('New source');
    await cdp.waitFor(`document.querySelector('dialog[open]') !== null`);
    await fillLabel('Name', 'Browser GH Source', 'dialog[open]');
    await clickText('Create', 'dialog[open]');
    await cdp.waitFor(`!document.querySelector('dialog[open]') && document.body.innerText.includes('Browser GH Source')`);
    assert.equal(await cdp.eval(`fetch('/api/v1/github-apps').then(r => r.json()).then(rows => rows.some(row => row.name === 'Browser GH Source'))`), true);

    await navigateWorkspace('security');
    await cdp.waitFor(`Boolean(document.querySelector('[data-testid="security-vuln-panel"]'))`);
    await cdp.eval(`(() => {
      const btn = Array.from(document.querySelectorAll('[data-testid="security-vuln-panel"] button')).find(b => b.textContent.includes('Image Scanning:'));
      btn.click();
    })()`);
    await cdp.waitFor(`document.body.innerText.includes('Image scanning enabled.')`);
    await cdp.screenshot('04-sources-security.png');
  });

  it('opens Docker Registries separately with server cards and a multi-server login dialog', async () => {
    await navigatePage('/registries', 'registries');
    await cdp.waitFor(`document.querySelector('main h1')?.textContent.trim() === 'Docker Registries'`);
    await cdp.waitFor(`document.querySelector('[data-testid="registry-server-card"]') !== null`);
    assert.equal(await cdp.eval(`document.body.innerText.includes('Add Server') || document.body.innerText.includes('Server Operations')`), false);
    await clickText('Multi-server login');
    await cdp.waitFor(`document.querySelector('[data-testid="registry-login-dialog"]')?.open`);
    assert.equal(await cdp.eval(`Boolean(document.querySelector('[data-testid="registry-login-dialog"] input[type="password"]'))`), true);
    await clickText('Cancel');
    await cdp.waitFor(`!document.querySelector('[data-testid="registry-login-dialog"]').open`);
    await navigatePage('/server/localhost/registries', 'server');
    await cdp.waitFor(`document.querySelector('[data-testid="registry-server-card"]') !== null`);
    assert.equal(await cdp.eval(`document.body.innerText.includes('This page has not been restored yet.')`), false);
    await cdp.screenshot('11-original-registries.png');
  });

  it('opens each notification channel separately and preserves its saved settings', async () => {
    await navigatePage('/notifications/discord', 'notifications');
    await cdp.waitFor(`document.querySelector('[data-testid="notification-settings"]')?.dataset.channel === 'discord'`);
    assert.equal(await cdp.eval(`Boolean(document.querySelector('[data-testid="governance-auth-panel"]'))`), false);
    assert.equal(await cdp.eval(`Boolean(document.querySelector('input[aria-label="Token label"]'))`), false);
    await fillLabel('Webhook URL', 'https://notify.example.test/discord');
    await clickText('Save');
    await cdp.waitFor(`document.body.innerText.includes('Saved.')`);
    await cdp.send('Page.reload');
    await cdp.waitFor(`document.querySelector('input[aria-label="Webhook URL"]')?.value === 'https://notify.example.test/discord'`);
    await navigatePage('/notifications/telegram', 'notifications');
    await cdp.waitFor(`document.querySelector('[data-testid="notification-settings"]')?.dataset.channel === 'telegram'`);
    assert.equal(await cdp.eval(`Boolean(document.querySelector('input[aria-label="Bot Token"]'))`), true);
    assert.equal(await cdp.eval(`Boolean(document.querySelector('input[aria-label="Webhook URL"]'))`), false);
    await navigatePage('/notifications/email', 'notifications');
    await cdp.waitFor(`document.querySelector('input[aria-label="Host"]') !== null`);
    await fillLabel('Host', 'smtp.example.test');
    await fillLabel('From Address', 'ops@example.test');
    await fillLabel('Recipients', 'team@example.test');
    await clickText('Save');
    await cdp.waitFor(`document.body.innerText.includes('Saved.')`);
    assert.equal(await cdp.eval(`fetch('/api/v1/notifications/email').then(r => r.json()).then(config => config.smtp_port)`), 587, 'the displayed default port must also be saved');
  });

  it('creates projects and resources through the catalogue and persists backup schedules, canvas positions and tags', async () => {
    // 1. Canvas real position persistence, opened using the dashboard view toggle.
    await navigateWorkspace('overview');
    await cdp.eval(`Array.from(document.querySelectorAll('.view-switcher button')).find(button => button.textContent.includes('Canvas')).click()`);
    await cdp.waitFor(`Boolean(document.querySelector('[data-testid="canvas-graph"]')) && !document.querySelector('[data-testid="canvas-graph"]').innerText.includes('(0 live nodes)')`);
    await cdp.eval(`(() => {
      const btn = Array.from(document.querySelectorAll('[data-testid="canvas-graph"] button')).find(b => b.textContent.includes('Save Layout'));
      btn.click();
    })()`);
    await cdp.waitFor(`document.body.innerText.includes('Canvas positions saved to PostgreSQL')`);

    // Projects, catalogue and per-resource configuration use their original deep links.
    await navigatePage('/projects', 'projects');
    await clickText('New project');
    await fillLabel('Name', 'Browser Resource Project', 'dialog[open]');
    await clickText('Create', 'dialog[open]');
    await cdp.waitFor(`location.pathname.startsWith('/project/') && document.querySelector('[data-testid="environments-page"]') !== null && document.body.innerText.includes('production')`);
    const context = await cdp.eval(`(async () => { const p = location.pathname.split('/')[2]; const envs = await fetch('/api/v1/projects/' + p + '/environments').then(r => r.json()); return { project: p, environment: envs[0].uuid }; })()`);
    const resourceBase = '/project/' + context.project + '/environment/' + context.environment;
    await navigatePage(resourceBase + '/new', 'environment');
    await cdp.waitFor(`document.querySelector('[data-testid="new-resource-page"]') !== null`);
    await clickText('Dockerfile');
    await cdp.waitFor(`Array.from(document.querySelectorAll('.catalogue-choice strong')).some(title => title.textContent === 'localhost')`);
    await cdp.eval(`Array.from(document.querySelectorAll('.catalogue-choice')).find(button => button.querySelector('strong')?.textContent === 'localhost').click()`);
    await fillLabel('Name', 'browser-live-app');
    await fillLabel('Dockerfile', 'FROM alpine:3.21\nCMD ["sleep", "infinity"]\n');
    await clickText('Continue');
    await cdp.waitFor(`document.querySelector('[data-testid="application-page"]') && document.querySelector('main h1')?.textContent === 'browser-live-app'`);
    applicationDeepLink = { ...context, uuid: await cdp.eval(`location.pathname.split('/')[6]`) };
    assert.equal(await cdp.eval(`Boolean(document.querySelector('[data-testid="new-resource-page"]'))`), false, 'configuration must not retain the creation catalogue');
    await navigatePage(resourceBase + '/new', 'environment');
    await clickText('PostgreSQL');
    await cdp.waitFor(`Array.from(document.querySelectorAll('.catalogue-choice strong')).some(title => title.textContent === 'localhost')`);
    await cdp.eval(`Array.from(document.querySelectorAll('.catalogue-choice')).find(button => button.querySelector('strong')?.textContent === 'localhost').click()`);
    await fillLabel('Name', 'browser-live-pg');
    await clickText('Continue');
    await cdp.waitFor(`document.querySelector('[data-testid="database-page"]') && document.querySelector('main h1')?.textContent === 'browser-live-pg'`);
    const dbPath = await cdp.eval('location.pathname');
    await navigatePage(dbPath + '/backups', 'database');
    await cdp.waitFor(`document.querySelector('[data-testid="backup-manager"]') !== null`);
    await cdp.eval(`document.querySelector('[data-testid="create-backup-schedule-btn"]').click()`);
    await cdp.waitFor(`document.body.innerText.includes('Backup schedule saved:')`);
    assert.equal(await cdp.eval(`fetch('/api/v1/databases/' + ${JSON.stringify(dbPath.split('/').at(-1))} + '/backups').then(r => r.json()).then(rows => rows.length > 0)`), true);
    await navigatePage('/tags', 'tags');
    await clickText('New tag');
    await fillLabel('Name', 'e2e-live-tag', 'dialog[open]');
    await clickText('Save', 'dialog[open]');
    await cdp.waitFor(`!document.querySelector('dialog[open]') && document.body.innerText.includes('e2e-live-tag')`);
    await cdp.screenshot('12-original-tags.png');
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

  it('opens an original application deep link after reload and loads that resource and environment', async () => {
    const { uuid, project, environment } = applicationDeepLink;
    const url = `${BASE_URL}/project/${project}/environment/${environment}/application/${uuid}/environment-variables`;
    await cdp.send('Page.navigate', { url });
    await cdp.waitFor(`window.__apiURLs?.includes('/api/v1/applications/${uuid}/envs') && document.querySelector('[data-testid="application-page"]') !== null`);
    assert.equal(await cdp.eval(`document.querySelector('main .settings-sidebar a').getAttribute('href')`), `/project/${project}/environment/${environment}/application/${uuid}`);
    await cdp.send('Page.reload');
    await cdp.waitFor(`window.__apiURLs?.includes('/api/v1/applications/${uuid}/envs')`);
  });

  it('edits a project with its gear action and opens the selected non-first server', async () => {
    await navigateWorkspace('overview');
    await cdp.waitFor(`document.querySelector('button[aria-label="Configure Browser E2E Project"]') !== null`);
    for (const table of ['Projects', 'Servers']) {
      const names = await cdp.eval(`Array.from(document.querySelectorAll('table[aria-label="${table}"] tbody .resource-name strong')).map(name => name.textContent)`);
      await cdp.eval(`document.querySelector('table[aria-label="${table}"] thead button').click()`);
      await cdp.waitFor(`document.querySelector('table[aria-label="${table}"] tbody .resource-name strong').textContent === ${JSON.stringify(names.at(-1))}`);
      const reversed = await cdp.eval(`Array.from(document.querySelectorAll('table[aria-label="${table}"] tbody .resource-name strong')).map(name => name.textContent)`);
      assert.deepEqual(reversed, [...names].reverse());
      await cdp.eval(`document.querySelector('table[aria-label="${table}"] thead button').click()`);
    }
    await cdp.eval(`document.querySelector('button[aria-label="Configure Browser E2E Project"]').click()`);
    await cdp.waitFor(`document.querySelector('#new-project-title').textContent === 'Edit project'`);
    await cdp.eval(`(() => { const input = document.querySelector('#project-description'); input.value = 'Dashboard configuration saved'; input.dispatchEvent(new Event('input', { bubbles: true })); input.closest('form').querySelector('button[type="submit"]').click(); })()`);
    await cdp.waitFor(`document.querySelector('table[aria-label="Projects"]').innerText.includes('Dashboard configuration saved')`);
    const saved = await cdp.eval(`fetch('/api/v1/projects').then(response => response.json()).then(projects => projects.find(project => project.name === 'Browser E2E Project').description)`);
    assert.equal(saved, 'Dashboard configuration saved');
    await cdp.eval(`Array.from(document.querySelectorAll('.servers-table .resource-name')).find(link => link.textContent.includes('edge-node-1')).click()`);
    await cdp.waitFor(`document.querySelector('[data-testid="server-page"]') && document.querySelector('main h1')?.textContent === 'edge-node-1'`);
    await cdp.eval(`document.querySelector('[data-testid="switch-team-20"]').click()`);
    await cdp.waitFor(`location.pathname === '/' && document.querySelector('[data-testid="active-team-id"]')?.dataset.teamId === '20' && document.body.innerText.includes('No servers in this team.')`);
    assert.equal(await cdp.eval(`document.body.innerText.includes('edge-node-1')`), false);
    await cdp.eval(`document.querySelector('[data-testid="switch-team-0"]').click()`);
    await cdp.waitFor(`location.pathname === '/' && document.querySelector('[data-testid="active-team-id"]')?.dataset.teamId === '0' && document.body.innerText.includes('edge-node-1')`);
  });

  it('uses the original navigation groups and searches actual team resources with Ctrl+K', async () => {
    await navigateWorkspace('overview');
    await cdp.waitFor(`document.querySelector('nav[aria-label="Primary navigation"] button[title="Settings"]') !== null`);
    const labels = await cdp.eval(`Array.from(document.querySelectorAll('nav[aria-label="Primary navigation"] .coolify-nav-link')).map(link => link.textContent.trim())`);
    assert.deepEqual(labels, ['Dashboard', 'Projects', 'Monitoring', 'Terminal', 'Servers', 'Integrations', 'Shared Variables', 'Team', 'Notifications', 'Keys & Tokens', 'Tags', 'Settings']);
    await cdp.eval(`document.querySelector('nav[aria-label="Primary navigation"] button[title="Monitoring"]').click()`);
    await cdp.waitFor(`document.querySelector('nav[aria-label="Primary navigation"] a[href="/metrics"]') !== null`);
    await cdp.eval(`document.querySelector('nav[aria-label="Primary navigation"] .nav-back').click()`);
    await cdp.eval(`document.querySelector('nav[aria-label="Primary navigation"] button[title="Integrations"]').click()`);
    await cdp.waitFor(`document.querySelector('nav[aria-label="Primary navigation"] a[href="/sources"]') !== null`);
    await cdp.eval(`document.querySelector('nav[aria-label="Primary navigation"] .nav-back').click()`);
    await cdp.send('Input.dispatchKeyEvent', { type: 'keyDown', key: 'k', code: 'KeyK', modifiers: 2, windowsVirtualKeyCode: 75 });
    await cdp.waitFor(`document.querySelector('dialog[aria-label="Search"]')?.open`);
    await cdp.eval(`(() => { const input = document.querySelector('#global-search'); input.value = 'edge-node-1'; input.dispatchEvent(new Event('input', { bubbles: true })); })()`);
    await cdp.waitFor(`document.querySelector('.search-results')?.innerText.includes('edge-node-1')`);
    await cdp.send('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
    await cdp.waitFor(`!document.querySelector('dialog[aria-label="Search"]').open`);
  });

  it('shows actual recent deployment metadata on the dashboard and retains the table layout on mobile', async () => {
    const { uuid } = applicationDeepLink;
    const response = await cdp.eval(`fetch('/api/v1/deploy?uuid=${uuid}', { method: 'POST', headers: { 'X-CSRF-TOKEN': decodeURIComponent(document.cookie.match(/(?:^|;\\s*)coolify_csrf=([^;]+)/)[1]) } }).then(response => response.status)`);
    assert.equal(response, 202);
    await navigateWorkspace('overview');
    await cdp.waitFor(`document.querySelector('.recent-deployments')?.innerText.includes('browser-live-app')`);
    assert.equal(await cdp.eval(`document.querySelector('.recent-deployments').innerText.includes('Queued')`), true);
    await cdp.send('Emulation.setDeviceMetricsOverride', { width: 1536, height: 864, deviceScaleFactor: 1, mobile: false });
    await cdp.screenshot('07-original-dashboard.png');
    await cdp.send('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 1, mobile: true });
    assert.equal(await cdp.eval(`window.innerWidth`), 390);
    assert.equal(await cdp.eval(`document.documentElement.scrollWidth`), 390);
    await cdp.screenshot('08-dashboard-mobile.png');
    await cdp.send('Emulation.clearDeviceMetricsOverride');
    await cdp.eval(`document.querySelector('.recent-deployment').click()`);
    await cdp.waitFor(`window.__apiURLs?.includes('/api/v1/deployments/applications/${uuid}') && document.body.innerText.includes('Deployment History') && document.body.innerText.includes('queued')`);
  });

  it('opens feedback from the mobile account menu without leaving the main page inert', async () => {
    await navigateWorkspace('overview');
    await cdp.send('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 1, mobile: true });
    await cdp.eval(`document.querySelector('[data-testid="mobile-menu-toggle"]').click()`);
    await cdp.waitFor(`document.querySelector('[data-testid="mobile-menu"]').open`);
    await cdp.eval(`document.querySelector('[data-testid="mobile-menu"] .account-trigger').click()`);
    await cdp.eval(`Array.from(document.querySelectorAll('[data-testid="mobile-menu"] .account-popover button')).find(button => button.textContent.includes('Help & Feedback')).click()`);
    await cdp.waitFor(`!document.querySelector('[data-testid="mobile-menu"]').open && document.querySelector('[data-testid="help-drawer"]') !== null`);
    await cdp.eval(`document.querySelector('[aria-label="Close feedback"]').click()`);
    await cdp.send('Emulation.clearDeviceMetricsOverride');
  });

  it('preserves navigation, theme and mobile layout using the original Coolify shell dimensions', async () => {
    await cdp.send('Page.navigate', { url: `${BASE_URL}/servers` });
    await cdp.waitFor(`document.querySelector('[data-workspace="servers"][data-hydrated="true"]') !== null`);
    const desktop = await cdp.eval(`(() => {
      const sidebar = document.querySelector('[data-testid="app-sidebar"]');
      const header = document.querySelector('[data-testid="app-topbar"]');
      return { sidebar: sidebar?.getBoundingClientRect().width, header: header?.getBoundingClientRect().height,
        active: document.querySelector('nav[aria-label="Primary navigation"] a[aria-current="page"]')?.getAttribute('href') };
    })()`);
    assert.equal(desktop.sidebar, 224);
    assert.equal(desktop.header, 48);
    assert.equal(desktop.active, '/servers');
    await cdp.eval(`document.querySelector('[data-testid="sidebar-collapse"]').click()`);
    await cdp.waitFor(`document.querySelector('[data-testid="app-sidebar"]').getBoundingClientRect().width === 64`);
    await cdp.send('Page.reload');
    await cdp.waitFor(`document.querySelector('[data-testid="app-sidebar"]')?.getBoundingClientRect().width === 64`);
    await cdp.waitFor(`document.querySelector('[data-testid="theme-toggle"]')?.closest('astro-island')?.hasAttribute('ssr') === false`);
    await cdp.eval(`document.querySelector('[data-testid="theme-toggle"]').click()`);
    await cdp.waitFor(`!document.documentElement.classList.contains('dark')`);
    await cdp.send('Page.reload');
    await cdp.waitFor(`!document.documentElement.classList.contains('dark')`);
    await cdp.send('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 1, mobile: true });
    await cdp.waitFor(`getComputedStyle(document.querySelector('[data-testid="mobile-menu-toggle"]')).display !== 'none'`);
    assert.equal(await cdp.eval(`document.documentElement.scrollWidth <= window.innerWidth`), true, 'mobile shell must not overflow');
    const mobileBounds = await cdp.eval(`({ width: window.innerWidth, x: window.scrollX, main: document.querySelector('main').getBoundingClientRect().left, viewport: window.visualViewport.width, offset: window.visualViewport.offsetLeft, overflow: [...document.querySelectorAll('main *')].map(el => ({tag: el.tagName, cls: el.className, style: el.getAttribute('style'), right: el.getBoundingClientRect().right})).filter(el => el.right > 391).slice(0, 12) })`);
    assert.equal(mobileBounds.width, 390, JSON.stringify(mobileBounds));
    assert.equal(mobileBounds.x, 0, JSON.stringify(mobileBounds));
    assert.equal(mobileBounds.main, 0, JSON.stringify(mobileBounds));
    assert.equal(mobileBounds.offset, 0, JSON.stringify(mobileBounds));
    await cdp.eval(`document.querySelector('[data-testid="mobile-menu-toggle"]').click()`);
    await cdp.waitFor(`document.querySelector('[data-testid="mobile-menu"]').open`);
    await cdp.send('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
    await cdp.waitFor(`!document.querySelector('[data-testid="mobile-menu"]').open`);
    await cdp.screenshot('06-mobile-shell.png');
    await cdp.send('Emulation.clearDeviceMetricsOverride');
    await cdp.eval(`document.querySelector('[data-testid="theme-toggle"]').click()`);
    await cdp.eval(`document.querySelector('[data-testid="sidebar-collapse"]').click()`);
  });

  it('logs out from mobile navigation, revokes PostgreSQL session, and returns 401 on subsequent /app/auth/me', async () => {
    await cdp.send('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 1, mobile: true });
    await cdp.eval(`document.querySelector('[data-testid="mobile-menu-toggle"]').click()`);
    await cdp.waitFor(`document.querySelector('[data-testid="mobile-menu"]').open && document.querySelector('[data-testid="mobile-menu"] button.coolify-btn') !== null`);
    await cdp.eval(`document.querySelector('[data-testid="mobile-menu"] .account-trigger').click()`);
    await cdp.eval(`(() => {
      const btns = Array.from(document.querySelectorAll('[data-testid="mobile-menu"] button'));
      const signOut = btns.find((b) => b.textContent.trim() === 'Sign out');
      if (!signOut) throw new Error('Sign out button not found');
      signOut.click();
    })()`);
    await cdp.waitFor(`window.location.pathname === '/login' && document.body.innerText.includes('Sign in to Goolify')`);

    const meStatus = await cdp.eval(`fetch('/app/auth/me', { credentials: 'same-origin' }).then(r => r.status)`);
    assert.equal(meStatus, 401, 'expected 401 from /app/auth/me after logout');
  });
});
