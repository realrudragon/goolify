<script setup lang="ts">
import { onMounted, ref, watch } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const props = withDefaults(defineProps<{ serverUuid?: string }>(), { serverUuid: 'localhost' });
const lifecycle = usePageLifecycle();

const cleanupThreshold = ref(80);
const sentinelEnabled = ref(false);
const logdrainAxiomEnabled = ref(false);
const logdrainAxiomDataset = ref('');
const cfTunnelEnabled = ref(false);

const registries = ref<Array<{ registry: string; username?: string; verified?: boolean }>>([]);
const newRegistry = ref('');
const newRegUser = ref('');
const newRegPass = ref('');

const certs = ref<Array<{ id: number; common_name: string; valid_until: string }>>([]);
const certCN = ref('');
const certPem = ref('');
const certKey = ref('');

const swarmDockers = ref<Array<{ uuid: string; name: string }>>([]);
const swarmName = ref('');

const runnerConfigured = ref(false);
const runnerExecs = ref<Array<{ uuid: string; status: string; workflow_name?: string }>>([]);
const hostScanSummary = ref<string>('Not scanned yet');
const status = ref('');

async function loadAll() {
  if (!props.serverUuid) return;
  const u = encodeURIComponent(props.serverUuid);
  try {
    const [dc, sent, ld, regs, certList, swList, runners, rExecs, hScan] = await Promise.all([
      apiRequest<{ docker_cleanup_threshold?: number }>(`/api/v1/servers/${u}/docker-cleanup`, { signal: lifecycle.signal }).catch(() => ({} as { docker_cleanup_threshold?: number })),
      apiRequest<{ is_sentinel_enabled?: boolean }>(`/api/v1/servers/${u}/sentinel`, { signal: lifecycle.signal }).catch(() => ({} as { is_sentinel_enabled?: boolean })),
      apiRequest<{ is_logdrain_axiom_enabled?: boolean; logdrain_axiom_dataset_name?: string }>(`/api/v1/servers/${u}/log-drains`, { signal: lifecycle.signal }).catch(() => ({} as { is_logdrain_axiom_enabled?: boolean; logdrain_axiom_dataset_name?: string })),
      apiRequest<Array<{ registry: string; username?: string; verified?: boolean }>>(`/api/v1/servers/${u}/registries`, { signal: lifecycle.signal }).catch(() => []),
      apiRequest<Array<{ id: number; common_name: string; valid_until: string }>>(`/api/v1/servers/${u}/certificates`, { signal: lifecycle.signal }).catch(() => []),
      apiRequest<Array<{ uuid: string; name: string }>>(`/api/v1/servers/${u}/swarm-dockers`, { signal: lifecycle.signal }).catch(() => []),
      apiRequest<{ configured?: boolean }>(`/api/v1/servers/${u}/github-runners`, { signal: lifecycle.signal }).catch(() => ({} as { configured?: boolean })),
      apiRequest<Array<{ uuid: string; status: string; workflow_name?: string }>>(`/api/v1/servers/${u}/github-runners/executions`, { signal: lifecycle.signal }).catch(() => []),
      apiRequest<{ scanned?: boolean; summary?: Record<string, unknown> }>(`/api/v1/servers/${u}/host-scan`, { signal: lifecycle.signal }).catch(() => ({} as { scanned?: boolean; summary?: Record<string, unknown> })),
    ]);
    if (typeof dc.docker_cleanup_threshold === 'number') cleanupThreshold.value = dc.docker_cleanup_threshold;
    sentinelEnabled.value = Boolean(sent.is_sentinel_enabled);
    logdrainAxiomEnabled.value = Boolean(ld.is_logdrain_axiom_enabled);
    logdrainAxiomDataset.value = ld.logdrain_axiom_dataset_name ?? '';
    registries.value = regs;
    certs.value = certList;
    swarmDockers.value = swList;
    runnerConfigured.value = Boolean(runners.configured);
    runnerExecs.value = rExecs;
    hostScanSummary.value = hScan.scanned ? JSON.stringify(hScan.summary ?? {}) : 'Not scanned yet';
  } catch {
    // Handled cleanly
  }
}

async function saveSubsystems() {
  const u = encodeURIComponent(props.serverUuid);
  status.value = '';
  try {
    await Promise.all([
      apiRequest(`/api/v1/servers/${u}/docker-cleanup`, {
        method: 'PATCH',
        signal: lifecycle.signal,
        body: JSON.stringify({ docker_cleanup_threshold: cleanupThreshold.value }),
      }),
      apiRequest(`/api/v1/servers/${u}/sentinel`, {
        method: 'PATCH',
        signal: lifecycle.signal,
        body: JSON.stringify({ is_sentinel_enabled: sentinelEnabled.value }),
      }),
      apiRequest(`/api/v1/servers/${u}/log-drains`, {
        method: 'PATCH',
        signal: lifecycle.signal,
        body: JSON.stringify({
          is_logdrain_axiom_enabled: logdrainAxiomEnabled.value,
          logdrain_axiom_dataset_name: logdrainAxiomDataset.value,
        }),
      }),
      apiRequest(`/api/v1/servers/${u}/cloudflare-tunnel`, {
        method: 'PATCH',
        signal: lifecycle.signal,
        body: JSON.stringify({ is_cloudflare_tunnel: cfTunnelEnabled.value }),
      }),
    ]);
    status.value = 'Server subsystems saved';
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function triggerCleanup() {
  const u = encodeURIComponent(props.serverUuid);
  try {
    await apiRequest(`/api/v1/servers/${u}/docker-cleanup`, { method: 'POST', signal: lifecycle.signal });
    status.value = 'Docker cleanup queued';
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function addRegistry() {
  if (!newRegistry.value.trim()) return;
  const u = encodeURIComponent(props.serverUuid);
  try {
    await apiRequest(`/api/v1/servers/${u}/registries`, {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({ registry: newRegistry.value.trim(), username: newRegUser.value.trim(), password: newRegPass.value }),
    });
    newRegistry.value = '';
    newRegUser.value = '';
    newRegPass.value = '';
    status.value = 'Registry saved';
    await loadAll();
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function addCert() {
  if (!certCN.value.trim() || !certPem.value.trim() || !certKey.value.trim()) return;
  const u = encodeURIComponent(props.serverUuid);
  try {
    await apiRequest(`/api/v1/servers/${u}/certificates`, {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({ common_name: certCN.value.trim(), ssl_certificate: certPem.value, ssl_private_key: certKey.value }),
    });
    certCN.value = '';
    certPem.value = '';
    certKey.value = '';
    status.value = 'SSL certificate encrypted and stored';
    await loadAll();
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function addSwarm() {
  if (!swarmName.value.trim()) return;
  const u = encodeURIComponent(props.serverUuid);
  try {
    await apiRequest(`/api/v1/servers/${u}/swarm-dockers`, {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({ name: swarmName.value.trim() }),
    });
    swarmName.value = '';
    status.value = 'Swarm Docker destination created';
    await loadAll();
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function runHostScan() {
  const u = encodeURIComponent(props.serverUuid);
  try {
    await apiRequest(`/api/v1/servers/${u}/host-scan`, {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({}),
    });
    status.value = 'Host security scan recorded';
    await loadAll();
  } catch (err) {
    status.value = (err as Error).message;
  }
}

watch(() => props.serverUuid, () => {
  void loadAll();
});

onMounted(() => {
  void loadAll();
});
</script>

<template>
  <div class="coolify-card" data-testid="server-ops-panel">
    <div class="coolify-well" style="display:grid;gap:12px;">
      <div style="display:flex;justify-content:space-between;align-items:center;">
        <strong>Server Operations ({{ serverUuid }})</strong>
        <div style="display:flex;gap:6px;">
          <button type="button" class="coolify-btn" @click="triggerCleanup">Run Docker Cleanup</button>
          <button type="button" class="coolify-btn" @click="runHostScan">Record Host Scan</button>
          <button type="button" class="coolify-btn" @click="loadAll">Refresh</button>
        </div>
      </div>

      <form style="display:flex;gap:10px;flex-wrap:wrap;align-items:center;" @submit.prevent="saveSubsystems">
        <label>Cleanup %: <input v-model.number="cleanupThreshold" type="number" class="coolify-input" style="width:76px;" /></label>
        <label><input v-model="sentinelEnabled" type="checkbox" /> Sentinel</label>
        <label><input v-model="logdrainAxiomEnabled" type="checkbox" /> Axiom Log Drain</label>
        <input v-model="logdrainAxiomDataset" class="coolify-input" placeholder="Axiom dataset" />
        <label><input v-model="cfTunnelEnabled" type="checkbox" /> Cloudflare Tunnel</label>
        <button type="submit" class="coolify-btn coolify-btn-primary">Save Subsystems</button>
      </form>

      <div style="display:grid;grid-template-columns:1fr 1fr;gap:10px;">
        <form style="display:grid;gap:6px;" @submit.prevent="addRegistry">
          <strong style="font-size:13px;">Container Registries ({{ registries.length }})</strong>
          <input v-model="newRegistry" class="coolify-input" placeholder="ghcr.io" required />
          <div style="display:flex;gap:6px;">
            <input v-model="newRegUser" class="coolify-input" placeholder="Username" />
            <input v-model="newRegPass" type="password" class="coolify-input" placeholder="Password" />
            <button type="submit" class="coolify-btn">Add</button>
          </div>
        </form>

        <form style="display:grid;gap:6px;" @submit.prevent="addSwarm">
          <strong style="font-size:13px;">Swarm Destinations ({{ swarmDockers.length }}) &amp; Runners</strong>
          <div style="display:flex;gap:6px;">
            <input v-model="swarmName" class="coolify-input" placeholder="Swarm cluster name" required />
            <button type="submit" class="coolify-btn">Add Swarm</button>
          </div>
          <span style="font-size:12px;color:var(--ui-fg-dim);">
            GitHub Runner: {{ runnerConfigured ? 'Configured' : 'Not configured' }} ({{ runnerExecs.length }} runs) | Host Scan: {{ hostScanSummary }}
          </span>
        </form>
      </div>

      <form style="display:flex;gap:6px;flex-wrap:wrap;" @submit.prevent="addCert">
        <input v-model="certCN" class="coolify-input" placeholder="SSL Common Name (app.example.com)" required />
        <input v-model="certPem" class="coolify-input" placeholder="PEM Certificate" required />
        <input v-model="certKey" type="password" class="coolify-input" placeholder="PEM Private Key" required />
        <button type="submit" class="coolify-btn">Store SSL Cert ({{ certs.length }})</button>
      </form>

      <p v-if="status" role="status" style="margin:0;">{{ status }}</p>
    </div>
  </div>
</template>
