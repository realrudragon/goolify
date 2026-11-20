<script setup lang="ts">
import { ref } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const props = withDefaults(defineProps<{ serverUuid?: string }>(), { serverUuid: 'localhost' });
const lifecycle = usePageLifecycle();
const proxyType = ref<'traefik' | 'caddy'>('traefik');
const preview = ref('');
const status = ref('');

async function loadProxy() {
  status.value = '';
  try {
    const res = await apiRequest<{ config?: string; proxy_type?: string }>(
      `/api/v1/servers/${encodeURIComponent(props.serverUuid)}/proxy?type=${proxyType.value}`,
      { signal: lifecycle.signal },
    );
    preview.value = JSON.stringify(res, null, 2);
    status.value = `Loaded ${proxyType.value} configuration`;
  } catch (err) {
    status.value = (err as Error).message;
  }
}
</script>

<template>
  <div class="coolify-card">
    <div class="coolify-well" style="display:grid;gap:8px;">
      <div style="display:flex;align-items:center;gap:10px;">
        <strong>Reverse Proxy</strong>
        <select v-model="proxyType" class="coolify-input" aria-label="Proxy Type">
          <option value="traefik">Traefik</option>
          <option value="caddy">Caddy</option>
        </select>
        <button type="button" class="coolify-btn" @click="loadProxy">Inspect Config</button>
      </div>
      <p v-if="status" style="margin:0;">{{ status }}</p>
      <pre v-if="preview" style="margin:0;font-family:var(--ui-font-mono);font-size:12px;overflow:auto;">{{ preview }}</pre>
    </div>
  </div>
</template>
