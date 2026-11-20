<script setup lang="ts">
import { onMounted, ref, watch } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

export interface ServerDto {
  id: number;
  uuid: string;
  name: string;
  ip: string;
  port: number;
  user: string;
  team_id: number;
  is_reachable: boolean;
  is_usable: boolean;
  transferred_at?: string | null;
}

const props = defineProps<{
  serverUuid: string;
}>();

const lifecycle = usePageLifecycle();
const server = ref<ServerDto | null>(null);
const loading = ref<boolean>(false);
const errorMessage = ref<string>('');

async function loadServer() {
  if (!props.serverUuid) {
    server.value = null;
    return;
  }
  loading.value = true;
  errorMessage.value = '';
  try {
    server.value = await apiRequest<ServerDto>(`/api/v1/servers/${encodeURIComponent(props.serverUuid)}`, {
      signal: lifecycle.signal,
    });
  } catch (err) {
    if ((err as Error).name !== 'AbortError') {
      server.value = null;
      errorMessage.value = (err as Error).message || 'Failed to load server summary';
    }
  } finally {
    loading.value = false;
  }
}

watch(
  () => props.serverUuid,
  () => {
    void loadServer();
  },
);

lifecycle.onTeamSwitch(() => {
  server.value = null;
  errorMessage.value = '';
});

onMounted(() => {
  void loadServer();
});
</script>

<template>
  <section class="coolify-card" aria-label="Server summary">
    <div class="coolify-well">
      <div style="display:flex;align-items:center;justify-content:space-between;gap:12px;">
        <strong>{{ server?.name ?? 'Server Summary' }}</strong>
        <button type="button" class="coolify-btn" @click="loadServer">Refresh</button>
      </div>
      <p v-if="loading">Loading server state...</p>
      <p v-else-if="errorMessage" role="alert" style="color:var(--ui-error);">{{ errorMessage }}</p>
      <dl v-else-if="server" style="display:grid;grid-template-columns:120px 1fr;gap:6px;margin-top:8px;">
        <dt>UUID</dt><dd>{{ server.uuid }}</dd>
        <dt>Address</dt><dd>{{ server.user }}@{{ server.ip }}:{{ server.port }}</dd>
        <dt>Status</dt><dd>{{ server.is_reachable ? 'Reachable' : 'Unreachable' }}</dd>
      </dl>
      <p v-else style="color:var(--ui-fg-dim);margin-top:8px;">Select a server to inspect details.</p>
    </div>
  </section>
</template>
