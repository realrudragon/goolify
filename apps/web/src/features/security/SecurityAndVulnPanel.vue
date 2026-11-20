<script setup lang="ts">
import { onMounted, ref, watch } from 'vue';
import { apiRequest } from '../../lib/api';

const props = defineProps<{
  serverUuid: string;
}>();

interface VulnScan {
  uuid: string;
  image_ref: string;
  status: string;
  summary?: Record<string, number>;
}

interface CleanupSettings {
  docker_cleanup_frequency: string;
  docker_cleanup_threshold: number;
  force_docker_cleanup: boolean;
  delete_unused_volumes: boolean;
}

const scans = ref<VulnScan[]>([]);
const cleanup = ref<CleanupSettings>({
  docker_cleanup_frequency: '0 0 * * *',
  docker_cleanup_threshold: 80,
  force_docker_cleanup: false,
  delete_unused_volumes: false,
});
const imageScanEnabled = ref(false);
const tunnelEnabled = ref(false);
const feedback = ref('');
const errMsg = ref('');

async function refresh() {
  errMsg.value = '';
  try {
    const [v, dc, isc, cf] = await Promise.all([
      apiRequest<VulnScan[]>('/api/v1/vulnerabilities').catch(() => []),
      apiRequest<CleanupSettings>(`/api/v1/servers/${encodeURIComponent(props.serverUuid)}/docker-cleanup`).catch(() => cleanup.value),
      apiRequest<{ is_image_scan_enabled: boolean }>(`/api/v1/servers/${encodeURIComponent(props.serverUuid)}/image-scanning`).catch(() => ({ is_image_scan_enabled: false })),
      apiRequest<{ is_cloudflare_tunnel: boolean }>(`/api/v1/servers/${encodeURIComponent(props.serverUuid)}/cloudflare-tunnel`).catch(() => ({ is_cloudflare_tunnel: false })),
    ]);
    scans.value = v ?? [];
    cleanup.value = dc;
    imageScanEnabled.value = Boolean(isc?.is_image_scan_enabled);
    tunnelEnabled.value = Boolean(cf?.is_cloudflare_tunnel);
  } catch (err) {
    errMsg.value = err instanceof Error ? err.message : 'Failed to load security state';
  }
}

async function runCleanup() {
  feedback.value = '';
  errMsg.value = '';
  try {
    await apiRequest(`/api/v1/servers/${encodeURIComponent(props.serverUuid)}/docker-cleanup/run`, {
      method: 'POST',
    });
    feedback.value = 'Docker cleanup queued.';
  } catch (err) {
    errMsg.value = err instanceof Error ? err.message : 'Failed to trigger cleanup';
  }
}

async function toggleImageScanning() {
  feedback.value = '';
  errMsg.value = '';
  try {
    const next = !imageScanEnabled.value;
    await apiRequest(`/api/v1/servers/${encodeURIComponent(props.serverUuid)}/image-scanning`, {
      method: 'PATCH',
      body: JSON.stringify({ is_image_scan_enabled: next }),
    });
    imageScanEnabled.value = next;
    feedback.value = `Image scanning ${next ? 'enabled' : 'disabled'}.`;
  } catch (err) {
    errMsg.value = err instanceof Error ? err.message : 'Update failed';
  }
}

async function toggleTunnel() {
  feedback.value = '';
  errMsg.value = '';
  try {
    const next = !tunnelEnabled.value;
    await apiRequest(
      `/api/v1/servers/${encodeURIComponent(props.serverUuid)}/cloudflare-tunnel/${next ? 'enable' : 'disable'}`,
      { method: 'POST' },
    );
    tunnelEnabled.value = next;
    feedback.value = `Cloudflare tunnel ${next ? 'enabled' : 'disabled'}.`;
  } catch (err) {
    errMsg.value = err instanceof Error ? err.message : 'Update failed';
  }
}

watch(() => props.serverUuid, () => {
  void refresh();
});

onMounted(() => {
  void refresh();
});
</script>

<template>
  <section class="coolify-card" data-testid="security-vuln-panel">
    <div class="coolify-well" style="display:grid;gap:12px;">
      <div style="display:flex;justify-content:space-between;align-items:center;">
        <h3 style="margin:0;font-size:15px;">Security, Image Scanning & Server Operations ({{ serverUuid }})</h3>
        <button type="button" class="coolify-btn" @click="refresh">Refresh</button>
      </div>

      <p v-if="feedback" style="margin:0;color:var(--ui-success);">{{ feedback }}</p>
      <p v-if="errMsg" style="margin:0;color:var(--ui-error);">{{ errMsg }}</p>

      <div style="display:flex;gap:8px;flex-wrap:wrap;align-items:center;">
        <button type="button" class="coolify-btn coolify-btn-primary" @click="runCleanup">Run Docker Cleanup</button>
        <button type="button" class="coolify-btn" @click="toggleImageScanning">
          Image Scanning: {{ imageScanEnabled ? 'ON' : 'OFF' }}
        </button>
        <button type="button" class="coolify-btn" @click="toggleTunnel">
          Cloudflare Tunnel: {{ tunnelEnabled ? 'ON' : 'OFF' }}
        </button>
        <span style="color:var(--ui-fg-dim);font-variant-numeric:tabular-nums;">
          Cleanup threshold: {{ cleanup.docker_cleanup_threshold }}% ({{ cleanup.docker_cleanup_frequency }})
        </span>
      </div>

      <div class="coolify-well" style="display:grid;gap:6px;">
        <strong>Fleet Image Vulnerability Scans ({{ scans.length }})</strong>
        <p v-if="scans.length === 0" style="margin:0;color:var(--ui-fg-dim);">No recorded image scans yet.</p>
        <ul v-else style="margin:0;padding-left:18px;font-family:var(--ui-font-mono);font-size:12px;">
          <li v-for="s in scans" :key="s.uuid">{{ s.image_ref }}: {{ s.status }} ({{ s.uuid }})</li>
        </ul>
      </div>
    </div>
  </section>
</template>
