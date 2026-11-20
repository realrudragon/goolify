<script setup lang="ts">
import { onMounted, ref } from 'vue';
import { apiRequest } from '../../lib/api';

interface GitAppItem {
  id: number;
  uuid: string;
  name: string;
  api_url?: string;
}

interface CloudTokenItem {
  uuid: string;
  provider: string;
  name: string;
}

interface IntegrationTokenItem {
  uuid: string;
  provider: string;
  name: string;
}

interface DnsZoneItem {
  uuid: string;
  name: string;
  provider: string;
}

const githubApps = ref<GitAppItem[]>([]);
const gitlabApps = ref<GitAppItem[]>([]);
const giteaApps = ref<GitAppItem[]>([]);
const cloudTokens = ref<CloudTokenItem[]>([]);
const integrationTokens = ref<IntegrationTokenItem[]>([]);
const dnsZones = ref<DnsZoneItem[]>([]);
const statusMsg = ref('');
const errorMsg = ref('');

const newGhName = ref('');
const newCloudProvider = ref('hetzner');
const newCloudName = ref('');
const newCloudToken = ref('');
const newIntProvider = ref('cloudflare');
const newIntName = ref('');
const newIntToken = ref('');

async function loadAll() {
  errorMsg.value = '';
  try {
    const [gh, gl, gt, ct, it, dz] = await Promise.all([
      apiRequest<GitAppItem[]>('/api/v1/github-apps').catch(() => []),
      apiRequest<GitAppItem[]>('/api/v1/gitlab-apps').catch(() => []),
      apiRequest<GitAppItem[]>('/api/v1/gitea-apps').catch(() => []),
      apiRequest<CloudTokenItem[]>('/api/v1/cloud-tokens').catch(() => []),
      apiRequest<IntegrationTokenItem[]>('/api/v1/security/integration-tokens').catch(() => []),
      apiRequest<DnsZoneItem[]>('/api/v1/dns/zones').catch(() => []),
    ]);
    githubApps.value = gh ?? [];
    gitlabApps.value = gl ?? [];
    giteaApps.value = gt ?? [];
    cloudTokens.value = ct ?? [];
    integrationTokens.value = it ?? [];
    dnsZones.value = dz ?? [];
  } catch (err) {
    errorMsg.value = err instanceof Error ? err.message : 'Failed to load integrations';
  }
}

async function createGithubApp() {
  if (!newGhName.value.trim()) return;
  statusMsg.value = '';
  errorMsg.value = '';
  try {
    await apiRequest('/api/v1/github-apps', {
      method: 'POST',
      body: JSON.stringify({ name: newGhName.value.trim() }),
    });
    newGhName.value = '';
    statusMsg.value = 'GitHub App created.';
    await loadAll();
  } catch (err) {
    errorMsg.value = err instanceof Error ? err.message : 'Create failed';
  }
}

async function createCloudToken() {
  if (!newCloudToken.value.trim()) return;
  statusMsg.value = '';
  errorMsg.value = '';
  try {
    await apiRequest('/api/v1/cloud-tokens', {
      method: 'POST',
      body: JSON.stringify({
        provider: newCloudProvider.value,
        name: newCloudName.value.trim() || newCloudProvider.value,
        token: newCloudToken.value.trim(),
      }),
    });
    newCloudName.value = '';
    newCloudToken.value = '';
    statusMsg.value = 'Cloud provider token stored (encrypted).';
    await loadAll();
  } catch (err) {
    errorMsg.value = err instanceof Error ? err.message : 'Create failed';
  }
}

async function createIntegrationToken() {
  if (!newIntToken.value.trim() || !newIntName.value.trim()) return;
  statusMsg.value = '';
  errorMsg.value = '';
  try {
    await apiRequest('/api/v1/security/integration-tokens', {
      method: 'POST',
      body: JSON.stringify({
        provider: newIntProvider.value,
        name: newIntName.value.trim(),
        token: newIntToken.value.trim(),
        capabilities: ['dns', 'secrets'],
      }),
    });
    newIntName.value = '';
    newIntToken.value = '';
    statusMsg.value = 'Integration token stored (encrypted).';
    await loadAll();
  } catch (err) {
    errorMsg.value = err instanceof Error ? err.message : 'Create failed';
  }
}

onMounted(() => {
  void loadAll();
});
</script>

<template>
  <section class="coolify-card" data-testid="sources-cloud-panel">
    <div class="coolify-well" style="display:grid;gap:12px;">
      <div style="display:flex;justify-content:space-between;align-items:center;">
        <h3 style="margin:0;font-size:15px;">Git Sources, Cloud Tokens & DNS Zones</h3>
        <button type="button" class="coolify-btn" @click="loadAll">Refresh</button>
      </div>

      <p v-if="statusMsg" style="margin:0;color:var(--ui-success);">{{ statusMsg }}</p>
      <p v-if="errorMsg" style="margin:0;color:var(--ui-error);">{{ errorMsg }}</p>

      <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">
        <div class="coolify-well" style="display:grid;gap:8px;">
          <strong>GitHub / GitLab / Gitea Sources ({{ githubApps.length + gitlabApps.length + giteaApps.length }})</strong>
          <div style="display:flex;gap:6px;">
            <input v-model="newGhName" class="coolify-input" placeholder="New GitHub App name" />
            <button type="button" class="coolify-btn coolify-btn-primary" @click="createGithubApp">Add GitHub App</button>
          </div>
          <ul style="margin:0;padding-left:18px;font-family:var(--ui-font-mono);font-size:12px;">
            <li v-for="g in githubApps" :key="g.uuid">[github] {{ g.name }} ({{ g.uuid }})</li>
            <li v-for="g in gitlabApps" :key="g.uuid">[gitlab] {{ g.name }} ({{ g.uuid }})</li>
            <li v-for="g in giteaApps" :key="g.uuid">[gitea] {{ g.name }} ({{ g.uuid }})</li>
          </ul>
        </div>

        <div class="coolify-well" style="display:grid;gap:8px;">
          <strong>Cloud Provider Tokens ({{ cloudTokens.length }})</strong>
          <div style="display:flex;gap:6px;flex-wrap:wrap;">
            <select v-model="newCloudProvider" class="coolify-input">
              <option value="hetzner">Hetzner</option>
              <option value="vultr">Vultr</option>
              <option value="digitalocean">DigitalOcean</option>
              <option value="hostinger">Hostinger</option>
            </select>
            <input v-model="newCloudName" class="coolify-input" placeholder="Label" />
            <input v-model="newCloudToken" type="password" class="coolify-input" placeholder="API Token" />
            <button type="button" class="coolify-btn coolify-btn-primary" @click="createCloudToken">Save Token</button>
          </div>
          <ul style="margin:0;padding-left:18px;font-family:var(--ui-font-mono);font-size:12px;">
            <li v-for="t in cloudTokens" :key="t.uuid">[{{ t.provider }}] {{ t.name }} ({{ t.uuid }})</li>
          </ul>
        </div>
      </div>

      <div class="coolify-well" style="display:grid;gap:8px;">
        <strong>Integration Tokens ({{ integrationTokens.length }}) & DNS Zones ({{ dnsZones.length }})</strong>
        <div style="display:flex;gap:6px;flex-wrap:wrap;">
          <select v-model="newIntProvider" class="coolify-input">
            <option value="cloudflare">Cloudflare</option>
            <option value="infisical">Infisical</option>
            <option value="vault">Vault</option>
          </select>
          <input v-model="newIntName" class="coolify-input" placeholder="Token name" />
          <input v-model="newIntToken" type="password" class="coolify-input" placeholder="Secret token" />
          <button type="button" class="coolify-btn coolify-btn-primary" @click="createIntegrationToken">Add Token</button>
        </div>
        <div v-if="dnsZones.length" style="font-family:var(--ui-font-mono);font-size:12px;">
          Zones: {{ dnsZones.map((z) => z.name).join(', ') }}
        </div>
      </div>
    </div>
  </section>
</template>
