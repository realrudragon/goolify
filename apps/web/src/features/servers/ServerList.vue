<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';
import type { ServerDto } from './ServerSummary.vue';

const emit = defineEmits<{ select: [uuid: string] }>();

const lifecycle = usePageLifecycle();
const servers = ref<ServerDto[]>([]);
const filter = ref<string>('');
const error = ref<string>('');

const filteredServers = computed(() => {
  const q = filter.value.trim().toLowerCase();
  if (!q) return servers.value;
  return servers.value.filter(
    (s) => s.name.toLowerCase().includes(q) || s.ip.toLowerCase().includes(q) || s.uuid.toLowerCase().includes(q),
  );
});

async function fetchList() {
  error.value = '';
  try {
    servers.value = await apiRequest<ServerDto[]>('/api/v1/servers', { signal: lifecycle.signal });
    if (servers.value.length > 0) {
      emit('select', servers.value[0].uuid);
    }
  } catch (err) {
    if ((err as Error).name !== 'AbortError') {
      error.value = (err as Error).message;
    }
  }
}

lifecycle.onTeamSwitch(() => {
  servers.value = [];
  void fetchList();
});

onMounted(() => {
  void fetchList();
});

defineExpose({ refresh: fetchList });
</script>

<template>
  <div class="coolify-card">
    <div class="coolify-well">
      <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:10px;gap:8px;">
        <strong>Managed Servers</strong>
        <input v-model="filter" class="coolify-input" placeholder="Filter servers..." aria-label="Filter servers" />
      </div>
      <p v-if="error" role="alert" style="color:var(--ui-error);">{{ error }}</p>
      <p v-else-if="filteredServers.length === 0" style="color:var(--ui-fg-dim);margin:0;">No servers in this team.</p>
      <ul v-else style="list-style:none;padding:0;margin:0;display:grid;gap:6px;">
        <li
          v-for="srv in filteredServers"
          :key="srv.uuid"
          style="display:flex;justify-content:space-between;align-items:center;padding:6px 8px;border:1px solid var(--ui-well-border);border-radius:6px;gap:8px;"
        >
          <a :href="`/server/${srv.uuid}`" @click.prevent="emit('select', srv.uuid)">{{ srv.name }} ({{ srv.ip }})</a>
          <span>{{ srv.is_reachable ? 'Online' : 'Offline' }}</span>
        </li>
      </ul>
    </div>
  </div>
</template>
