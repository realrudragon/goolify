<script setup lang="ts">
import { ref } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const props = defineProps<{ serverUuid: string }>();
const lifecycle = usePageLifecycle();
const cleanupEnabled = ref(true);
const buildServer = ref(false);
const status = ref('');

async function saveSettings() {
  try {
    await apiRequest(`/api/v1/servers/${encodeURIComponent(props.serverUuid)}`, {
      method: 'PATCH',
      signal: lifecycle.signal,
      body: JSON.stringify({
        is_build_server: buildServer.value,
        docker_cleanup_enabled: cleanupEnabled.value,
      }),
    });
    status.value = 'Settings updated';
  } catch (err) {
    status.value = (err as Error).message;
  }
}
</script>

<template>
  <form class="coolify-card" @submit.prevent="saveSettings">
    <div class="coolify-well" style="display:grid;gap:8px;">
      <strong>Server Settings</strong>
      <label><input v-model="buildServer" type="checkbox" /> Use as Build Server</label>
      <label><input v-model="cleanupEnabled" type="checkbox" /> Automated Docker Cleanup</label>
      <button type="submit" class="coolify-btn coolify-btn-primary">Update Settings</button>
      <p v-if="status">{{ status }}</p>
    </div>
  </form>
</template>
