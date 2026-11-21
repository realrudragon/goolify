import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import path from 'node:path';

const root = path.resolve(import.meta.dirname, '../..');
for (const compose of ['compose.yml', 'docker/stack-migration/docker-compose.yml', 'docker/stack-migration/docker-compose.cutover.yml']) {
  test(`${compose} prevents the non-HTTP worker from inheriting the image HTTP probe`, () => {
    const config = JSON.parse(execFileSync('docker', ['compose', '-f', compose, 'config', '--format', 'json'], {
      cwd: root,
      env: { ...process.env, APP_KEY: 'test-key', REHEARSAL_DB_URL: 'postgres://test:test@localhost/test', REHEARSAL_REDIS_URL: 'redis://localhost:6379' },
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
    }));
    assert.equal(config.services.worker.healthcheck?.disable, true);
    assert.notEqual(config.services.controlplane.healthcheck?.disable, true);
    assert.ok(config.services.worker.entrypoint.includes('/usr/local/bin/coolify-worker'));
  });
}
