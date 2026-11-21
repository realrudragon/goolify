import assert from 'node:assert/strict';
import { test } from 'node:test';
import { resolveUIRoute } from '../../apps/web/src/lib/ui-route.ts';

test('opens the resource and project context from original Coolify deep links', () => {
  const route = resolveUIRoute('/project/p-one/environment/e-one/application/a-one/environment-variables');
  assert.equal(route.section, 'projects');
  assert.equal(route.projectUuid, 'p-one');
  assert.equal(route.environmentUuid, 'e-one');
  assert.equal(route.applicationUuid, 'a-one');
  assert.equal(resolveUIRoute('/project/p/environment/e/service/s/configuration').serviceUuid, 's');
  const db = resolveUIRoute('/project/p/environment/e/database/d/general');
  assert.equal(db.section, 'databases');
  assert.equal(db.databaseUuid, 'd');
});

test('maps original infrastructure, canvas, token and settings routes to their workspaces', () => {
  assert.equal(resolveUIRoute('/server/srv-one/proxy').serverUuid, 'srv-one');
  assert.equal(resolveUIRoute('/servers/srv-two').serverUuid, 'srv-two');
  assert.equal(resolveUIRoute('/servers/canvas').serverUuid, undefined);
  assert.equal(resolveUIRoute('/dashboard/canvas').section, 'overview');
  assert.equal(resolveUIRoute('/canvas/tags').section, 'overview');
  assert.equal(resolveUIRoute('/security/api-tokens').section, 'settings');
  assert.equal(resolveUIRoute('/security/cloud-tokens').section, 'sources');
  assert.equal(resolveUIRoute('/registries').section, 'servers');
  assert.equal(resolveUIRoute('/profile/appearance').section, 'settings');
});

test('handles encoded and malformed paths without crashing or confusing route identifiers', () => {
  assert.equal(resolveUIRoute('/server/node%2Done/general').serverUuid, 'node-one');
  assert.equal(resolveUIRoute('/server/%E0%A4/general').serverUuid, undefined);
  assert.equal(resolveUIRoute('/server/a%2Fb/general').serverUuid, undefined);
  assert.equal(resolveUIRoute('/not-a-route').section, 'overview');
});
