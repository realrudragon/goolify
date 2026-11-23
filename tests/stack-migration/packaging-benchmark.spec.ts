import { describe, it, before, after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { execFileSync, spawn } from 'node:child_process';

const rootDir = path.resolve(import.meta.dirname, '../..');
const backendDir = path.join(rootDir, 'backend');
const distDir = path.join(rootDir, 'apps/web/dist');
const receiptPath = path.join(rootDir, 'migration/receipts/benchmark-report.json');

const DB_DSN = process.env.TEST_DATABASE_URL || 'postgres://coolify:coolify_test_pw@127.0.0.1:15432/coolify_test?sslmode=disable';
const REDIS_URL = process.env.TEST_REDIS_URL || 'redis://127.0.0.1:16379/0';
const APP_KEY = 'base64:MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=';
const BENCH_PORT = 18532;
const BASE_URL = `http://127.0.0.1:${BENCH_PORT}`;
const IMAGE_TAG = process.env.COOLIFY_STANDALONE_IMAGE || 'coolify-standalone-bench:local';

function runCmd(cmd: string, args: string[], opts: Record<string, unknown> = {}): string {
  return execFileSync(cmd, args, { encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'], ...opts }).trim();
}

function psqlExec(sql: string): void {
  execFileSync('docker', [
    'exec', 'coolify-mig-test-pg',
    'psql', '-U', 'coolify', '-d', 'coolify_test', '-v', 'ON_ERROR_STOP=1', '-c', sql,
  ], { stdio: 'pipe' });
}

async function waitForHTTP(url: string, timeoutMs = 25000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(url);
      if (res.ok) return;
    } catch {
      // still booting
    }
    await new Promise((r) => setTimeout(r, 150));
  }
  throw new Error(`Timed out waiting for ${url}`);
}

function percentile(sorted: number[], p: number): number {
  if (sorted.length === 0) return 0;
  const idx = Math.min(sorted.length - 1, Math.max(0, Math.ceil((p / 100) * sorted.length) - 1));
  return Number(sorted[idx].toFixed(2));
}

function walkFiles(dir: string, predicate: (fullPath: string) => boolean, out: string[] = []): string[] {
  if (!fs.existsSync(dir)) return out;
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    if (entry.name === '.git' || entry.name === 'node_modules' || entry.name === 'dist' || entry.name === 'target') {
      continue;
    }
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      walkFiles(full, predicate, out);
    } else if (predicate(full)) {
      out.push(path.relative(rootDir, full).replace(/\\/g, '/'));
    }
  }
  return out;
}

describe('Task 14: Packaging, Performance Benchmarks & Legacy Runtime Cleanliness', () => {
  let tmpDir = '';
  let cpBin = '';
  let workerBin = '';
  let cpProc: ReturnType<typeof spawn> | null = null;
  let workerProc: ReturnType<typeof spawn> | null = null;
  const skipDockerBuild = process.env.SKIP_DOCKER_BUILD === '1';

  before(() => {
    tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'coolify-bench-'));
    const ext = process.platform === 'win32' ? '.exe' : '';
    cpBin = path.join(tmpDir, `coolify-controlplane${ext}`);
    workerBin = path.join(tmpDir, `coolify-worker${ext}`);
  });

  after(() => {
    if (cpProc && !cpProc.killed) cpProc.kill('SIGKILL');
    if (workerProc && !workerProc.killed) workerProc.kill('SIGKILL');
    try {
      fs.rmSync(tmpDir, { recursive: true, force: true });
    } catch {
      // ignore cleanup lock on Windows
    }
  });

  it('verifies legacy PHP/Laravel/Livewire/Reverb/Node-terminal/Svelte files are removed while runtime assets remain', () => {
    const removalListPath = path.join(rootDir, 'migration/legacy-removal-list.txt');
    assert.ok(fs.existsSync(removalListPath), 'migration/legacy-removal-list.txt must exist');

    const listedPaths = fs.readFileSync(removalListPath, 'utf8')
      .split(/\r?\n/)
      .map((l) => l.trim())
      .filter((l) => l && !l.startsWith('#'));
    assert.ok(listedPaths.length >= 20, 'legacy-removal-list.txt must enumerate legacy artifacts');

    const stillPresent = listedPaths.filter((rel) => fs.existsSync(path.join(rootDir, rel)));
    assert.deepEqual(
      stillPresent,
      [],
      `Legacy paths still exist on disk: ${stillPresent.join(', ')}`
    );

    const remainingPhp = walkFiles(rootDir, (f) => f.endsWith('.php'));
    assert.deepEqual(remainingPhp, [], `Unexpected .php files remain: ${remainingPhp.join(', ')}`);

    const remainingSvelte = walkFiles(rootDir, (f) => f.endsWith('.svelte'));
    assert.deepEqual(remainingSvelte, [], `Unexpected .svelte files remain: ${remainingSvelte.join(', ')}`);

    // Verify essential runtime & historical assets remain intact
    for (const keep of [
      'templates/compose',
      'tests/Fixtures/parser/service-reference.yaml',
      'tests/stack-migration/inventory.test.ts',
      'migration/features.csv',
      'migration/routes.csv',
      'migration/data-coverage.csv',
      'migration/runbook.md',
      'migration/rollback.md',
      'apps/web/package.json',
      'apps/web/public/favicon.ico',
      'backend/go.mod',
      'executor/Cargo.toml',
      'docker/stack-migration/Dockerfile',
      'docker/stack-migration/docker-compose.yml',
      'DESIGN.md',
      'SECURITY.md',
      'TECH_STACK.md',
      'release.json',
      'CHANGELOG.md',
    ]) {
      assert.ok(fs.existsSync(path.join(rootDir, keep)), `Required preserved asset missing: ${keep}`);
    }

    const techStack = fs.readFileSync(path.join(rootDir, 'TECH_STACK.md'), 'utf8');
    assert.match(techStack, /Astro/i);
    assert.match(techStack, /Vue 3\.5/i);
    assert.match(techStack, /Go/i);
    assert.match(techStack, /Rust/i);
    assert.doesNotMatch(techStack, /\bLaravel 11\b|\bLivewire\b|\bPHP 8\.4\b/i);
  });

  it('verifies multi-stage standalone Docker packaging: non-root, persistent /data/coolify, zero PHP/Node/Svelte, image size budget', () => {
    const dockerfile = fs.readFileSync(path.join(rootDir, 'docker/stack-migration/Dockerfile'), 'utf8');
    assert.match(dockerfile, /USER coolify/);
    assert.match(dockerfile, /\/data\/coolify/);
    assert.match(dockerfile, /coolify-controlplane/);
    assert.match(dockerfile, /coolify-worker/);
    assert.match(dockerfile, /coolify-migrate/);
    assert.match(dockerfile, /coolify-cli/);
    assert.match(dockerfile, /coolify-executor/);

    const compose = fs.readFileSync(path.join(rootDir, 'docker/stack-migration/docker-compose.yml'), 'utf8');
    assert.match(compose, /(?:coolifydata|\/data\/coolify):\/data\/coolify/);
    assert.match(compose, /coolify:go:v1:/);
    assert.doesNotMatch(compose, /php|horizon|reverb|laravel/i);

    if (!skipDockerBuild) {
      execFileSync('docker', ['build', '-f', 'docker/stack-migration/Dockerfile', '-t', IMAGE_TAG, '.'], {
        cwd: rootDir,
        stdio: 'pipe',
      });
    }

    const sizeBytes = Number(runCmd('docker', ['image', 'inspect', IMAGE_TAG, '--format', '{{.Size}}']));
    const sizeMB = Number((sizeBytes / (1024 * 1024)).toFixed(2));
    assert.ok(sizeMB > 10 && sizeMB <= 250, `Expected standalone image <= 250 MB, got ${sizeMB} MB`);

    const inspectScript = [
      'set -e',
      'echo "UID=$(id -u)"',
      'for b in php composer node npm npx svelte; do',
      '  if command -v "$b" >/dev/null 2>&1; then echo "FORBIDDEN=$b"; exit 1; fi',
      'done',
      'for b in coolify-controlplane coolify-worker coolify-migrate coolify-cli coolify-executor; do',
      '  test -x "/usr/local/bin/$b"',
      'done',
      'test -d /data/coolify && test -w /data/coolify',
      'test -f /app/web/dist/index.html && test -f /app/web/dist/login/index.html && test -f /app/web/dist/favicon.ico',
      'echo "CONTAINER_AUDIT_OK=true"',
    ].join('\n');

    const out = execFileSync('docker', ['run', '-i', '--rm', '--entrypoint', 'sh', IMAGE_TAG], {
      input: inspectScript,
      encoding: 'utf8',
    });
    assert.match(out, /UID=1000/);
    assert.match(out, /CONTAINER_AUDIT_OK=true/);
  });

  it('measures real API p95/p99 latency, concurrent worker deploy throughput, SSE log rate, terminal latency, RAM & restart recovery', async () => {
    // 1. Clean isolated DB & compile real production Go binaries
    psqlExec('DROP SCHEMA public CASCADE; CREATE SCHEMA public;');
    execFileSync('go', ['build', '-o', cpBin, './cmd/controlplane'], { cwd: backendDir, stdio: 'pipe' });
    execFileSync('go', ['build', '-o', workerBin, './cmd/worker'], { cwd: backendDir, stdio: 'pipe' });

    const startControlPlane = () => spawn(cpBin, [
      '-addr', `127.0.0.1:${BENCH_PORT}`,
      '-static-dir', distDir,
      '-bootstrap-schema=true',
    ], {
      env: {
        ...process.env,
        DATABASE_URL: DB_DSN,
        REDIS_URL: REDIS_URL,
        REDIS_PREFIX: 'coolify:go:v1:bench:',
        APP_KEY: APP_KEY,
        ROOT_USER_EMAIL: 'root@coolify.test',
        ROOT_USER_PASSWORD: 'RootSecret123!',
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    });

    cpProc = startControlPlane();
    await waitForHTTP(`${BASE_URL}/api/v1/health`, 25000);

    workerProc = spawn(workerBin, ['-server-concurrency', '4'], {
      env: {
        ...process.env,
        DATABASE_URL: DB_DSN,
        REDIS_URL: REDIS_URL,
        REDIS_PREFIX: 'coolify:go:v1:bench:',
        APP_KEY: APP_KEY,
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    });

    const sampleRssMB = (pid: number | undefined): number => {
      if (!pid) return 0;
      try {
        if (process.platform === 'win32') {
          const out = runCmd('powershell', ['-NoProfile', '-Command', `(Get-Process -Id ${pid}).WorkingSet64`]);
          return Number((Number(out) / (1024 * 1024)).toFixed(2));
        }
        const out = runCmd('ps', ['-o', 'rss=', '-p', String(pid)]);
        return Number((Number(out) / 1024).toFixed(2));
      } catch {
        return 0;
      }
    };

    const idleRssMB = sampleRssMB(cpProc.pid);

    // 2. Authenticate via real POST /app/auth/login -> PostgreSQL session + CSRF
    const loginRes = await fetch(`${BASE_URL}/app/auth/login`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email: 'root@coolify.test', password: 'RootSecret123!' }),
    });
    assert.equal(loginRes.status, 200);
    const setCookies = loginRes.headers.getSetCookie ? loginRes.headers.getSetCookie() : [];
    const cookieHeader = setCookies.map((c) => c.split(';')[0]).join('; ');
    const csrfCookie = setCookies.find((c) => c.startsWith('coolify_csrf=')) || '';
    const csrfToken = decodeURIComponent((csrfCookie.split(';')[0].split('=')[1]) || '');
    assert.ok(cookieHeader.includes('coolify_session='), 'expected HttpOnly session cookie');

    // Create a Bearer token for API latency benchmark
    const tokRes = await fetch(`${BASE_URL}/app/auth/tokens`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Cookie: cookieHeader,
        'X-CSRF-Token': csrfToken,
      },
      body: JSON.stringify({ name: 'bench-token', abilities: ['read', 'write', 'deploy', 'root'] }),
    });
    assert.equal(tokRes.status, 201);
    const tokBody = await tokRes.json() as { token: string };
    const authHeaders = {
      Authorization: `Bearer ${tokBody.token}`,
      'Content-Type': 'application/json',
    };

    // 3. Warm up pgx connection pool (5 requests), then measure steady-state API latency p50 / p95 / p99 over 100 live PostgreSQL-backed requests
    for (let w = 0; w < 5; w++) {
      const r = await fetch(`${BASE_URL}/api/v1/servers`, { headers: authHeaders });
      assert.equal(r.status, 200);
      await r.text();
    }
    const latencies: number[] = [];
    for (let i = 0; i < 100; i++) {
      const target = i % 3 === 0 ? '/api/v1/servers' : i % 3 === 1 ? '/api/v1/projects' : '/api/v1/health';
      const t0 = performance.now();
      const res = await fetch(`${BASE_URL}${target}`, { headers: authHeaders });
      assert.equal(res.status, 200);
      await res.text();
      latencies.push(performance.now() - t0);
    }
    latencies.sort((a, b) => a - b);
    const apiP50 = percentile(latencies, 50);
    const apiP95 = percentile(latencies, 95);
    const apiP99 = percentile(latencies, 99);
    assert.ok(apiP50 < 150, `API p50 latency exceeded budget: ${apiP50}ms`);
    assert.ok(apiP95 < 400, `API p95 latency exceeded budget: ${apiP95}ms`);
    assert.ok(apiP99 < 800, `API p99 latency exceeded budget: ${apiP99}ms`);

    // 4. Create project + app and benchmark 10 concurrent deployments drained by real coolify-worker
    const projRes = await fetch(`${BASE_URL}/api/v1/projects`, {
      method: 'POST',
      headers: authHeaders,
      body: JSON.stringify({ name: 'Bench Project', description: 'Task 14 benchmark' }),
    });
    assert.equal(projRes.status, 201);
    const projBody = await projRes.json() as { project: { uuid: string }; environment?: { uuid: string } };
    const projUUID = projBody.project.uuid;
    let envUUID = projBody.environment?.uuid || '';
    if (!envUUID) {
      const envsRes = await fetch(`${BASE_URL}/api/v1/projects/${projUUID}/environments`, { headers: authHeaders });
      assert.equal(envsRes.status, 200);
      const envs = await envsRes.json() as Array<{ uuid: string }>;
      envUUID = envs[0]?.uuid || '';
    }
    assert.ok(envUUID, 'expected production environment uuid');

    const appRes = await fetch(`${BASE_URL}/api/v1/applications/dockerimage`, {
      method: 'POST',
      headers: authHeaders,
      body: JSON.stringify({
        environment_uuid: envUUID,
        server_uuid: 'localhost',
        name: 'bench-app',
        docker_registry_image_name: 'nginx',
        docker_registry_image_tag: 'alpine',
      }),
    });
    assert.equal(appRes.status, 201);
    const appBody = await appRes.json() as { uuid: string };
    const appUUID = appBody.uuid;

    const deployStart = performance.now();
    const deployPromises = Array.from({ length: 10 }, () =>
      fetch(`${BASE_URL}/api/v1/deploy`, {
        method: 'POST',
        headers: authHeaders,
        body: JSON.stringify({ uuid: appUUID, force: true }),
      }).then(async (r) => {
        assert.ok(r.status === 200 || r.status === 201 || r.status === 202, `unexpected deploy status ${r.status}`);
        return r.json();
      })
    );
    await Promise.all(deployPromises);

    // Wait for real coolify-worker to process all 10 queued jobs in Postgres
    const drainDeadline = Date.now() + 60000;
    let processedCount = 0;
    while (Date.now() < drainDeadline) {
      const out = runCmd('docker', [
        'exec', 'coolify-mig-test-pg',
        'psql', '-U', 'coolify', '-d', 'coolify_test', '-tA',
        '-c', "SELECT COUNT(*) FROM go_job_operations WHERE status IN ('finished','failed','cancelled');",
      ]);
      processedCount = Number(out.trim());
      if (processedCount >= 10) break;
      await new Promise((r) => setTimeout(r, 150));
    }
    const deployElapsedSec = Math.max(0.01, (performance.now() - deployStart) / 1000);
    const deployThroughputPerSec = Number((processedCount / deployElapsedSec).toFixed(2));
    assert.ok(processedCount >= 10, `Expected worker to process 10 concurrent deploy jobs, processed ${processedCount}`);

    // 5. Measure live SSE log streaming throughput (100 persisted lines streamed via /realtime/logs)
    psqlExec(`
      INSERT INTO go_operation_logs (resource_key, operation_id, stream, text)
      SELECT 'localhost', 'op-bench-sse', 'stdout', 'BENCH_LOG_LINE_' || g
      FROM generate_series(1, 100) AS g;
    `);
    const sseStart = performance.now();
    const sseAbort = new AbortController();
    const sseRes = await fetch(`${BASE_URL}/realtime/logs?resource=localhost&operation_id=op-bench-sse`, {
      headers: { Authorization: `Bearer ${tokBody.token}` },
      signal: sseAbort.signal,
    });
    assert.equal(sseRes.status, 200);
    let sseReceived = 0;
    const reader = sseRes.body!.getReader();
    const decoder = new TextDecoder();
    let sseBuf = '';
    while (sseReceived < 100) {
      const { value, done } = await reader.read();
      if (done) break;
      sseBuf += decoder.decode(value, { stream: true });
      const matches = sseBuf.match(/BENCH_LOG_LINE_/g);
      sseReceived = matches ? matches.length : 0;
    }
    sseAbort.abort();
    const sseElapsedSec = Math.max(0.001, (performance.now() - sseStart) / 1000);
    const sseLinesPerSec = Number((sseReceived / sseElapsedSec).toFixed(1));
    assert.ok(sseReceived >= 100, `Expected 100 SSE log events, received ${sseReceived}`);

    // 6. Measure live WebSocket terminal ticket + round-trip command latency
    const ticketRes = await fetch(`${BASE_URL}/app/terminal/ticket`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Cookie: cookieHeader,
        'X-CSRF-Token': csrfToken,
      },
      body: JSON.stringify({ server_uuid: 'localhost' }),
    });
    assert.equal(ticketRes.status, 201);
    const ticketBody = await ticketRes.json() as { ticket: string };
    const termLatencyMs = await new Promise<number>((resolve, reject) => {
      const t0 = performance.now();
      const ws = new WebSocket(
        `ws://127.0.0.1:${BENCH_PORT}/terminal/ws?ticket=${encodeURIComponent(ticketBody.ticket)}`,
        { headers: { Cookie: cookieHeader } } as unknown as string[]
      );
      const timer = setTimeout(() => { ws.close(); reject(new Error('terminal WS timeout')); }, 10000);
      ws.addEventListener('open', () => {
        ws.send(JSON.stringify({ type: 'input', data: 'echo TERM_BENCH_OK\n' }));
      });
      ws.addEventListener('message', (ev) => {
        if (String(ev.data).includes('TERM_BENCH_OK')) {
          clearTimeout(timer);
          ws.close();
          resolve(Number((performance.now() - t0).toFixed(2)));
        }
      });
      ws.addEventListener('error', (err) => {
        clearTimeout(timer);
        reject(err);
      });
    });

    const activeRssMB = sampleRssMB(cpProc.pid);
    const workerRssMB = sampleRssMB(workerProc.pid);

    // 7. Restart recovery test: kill controlplane, reboot against same Postgres/Redis, confirm session + project survive
    cpProc.kill('SIGKILL');
    await new Promise((r) => setTimeout(r, 400));
    cpProc = startControlPlane();
    await waitForHTTP(`${BASE_URL}/api/v1/health`, 25000);

    const meAfterRestart = await fetch(`${BASE_URL}/app/auth/me`, {
      headers: { Cookie: cookieHeader },
    });
    assert.equal(meAfterRestart.status, 200, 'PostgreSQL session must survive controlplane restart');

    const projAfterRestart = await fetch(`${BASE_URL}/api/v1/projects/${projUUID}`, {
      headers: authHeaders,
    });
    assert.equal(projAfterRestart.status, 200, 'Persisted project must survive controlplane restart');

    let imageSizeMB = 0;
    try {
      const sz = Number(runCmd('docker', ['image', 'inspect', IMAGE_TAG, '--format', '{{.Size}}']));
      imageSizeMB = Number((sz / (1024 * 1024)).toFixed(2));
    } catch {
      imageSizeMB = 0;
    }

    const report = {
      measured_at: new Date().toISOString(),
      stack: 'standalone-astro-vue3.5-go-rust',
      image_size_mb: imageSizeMB,
      controlplane_idle_rss_mb: idleRssMB,
      controlplane_active_rss_mb: activeRssMB,
      worker_rss_mb: workerRssMB,
      api_latency_ms: {
        samples: latencies.length,
        p50: apiP50,
        p95: apiP95,
        p99: apiP99,
      },
      concurrent_deployments: {
        queued: 10,
        processed: processedCount,
        elapsed_sec: Number(deployElapsedSec.toFixed(2)),
        throughput_per_sec: deployThroughputPerSec,
      },
      sse_log_streaming: {
        lines_received: sseReceived,
        lines_per_sec: sseLinesPerSec,
      },
      terminal_roundtrip_ms: termLatencyMs,
      restart_recovery_verified: true,
    };

    fs.mkdirSync(path.dirname(receiptPath), { recursive: true });
    fs.writeFileSync(receiptPath, JSON.stringify(report, null, 2) + '\n', 'utf8');
  });
});
