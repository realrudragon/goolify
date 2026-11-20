<script setup lang="ts">
import { onMounted, ref, watch } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const props = defineProps<{ applicationUuid: string }>();
const lifecycle = usePageLifecycle();

interface DeploymentRow {
  deployment_uuid: string;
  status: string;
  created_at: string;
}

const rows = ref<DeploymentRow[]>([]);

async function loadHistory() {
  if (!props.applicationUuid) {
    rows.value = [];
    return;
  }
  try {
    rows.value = await apiRequest<DeploymentRow[]>(
      `/api/v1/deployments/applications/${encodeURIComponent(props.applicationUuid)}`,
      { signal: lifecycle.signal },
    );
  } catch {
    rows.value = [];
  }
}

watch(() => props.applicationUuid, () => {
  void loadHistory();
});

lifecycle.onTeamSwitch(() => {
  rows.value = [];
});

onMounted(() => {
  void loadHistory();
});

defineExpose({ refresh: loadHistory });
</script>

<template>
  <div class="coolify-card">
    <div class="coolify-well" style="display:grid;gap:8px;">
      <div style="display:flex;justify-content:space-between;align-items:center;">
        <strong>Deployment History</strong>
        <button type="button" class="coolify-btn" @click="loadHistory">Refresh</button>
      </div>
      <p v-if="rows.length === 0" style="color:var(--ui-fg-dim);margin:0;">No deployments recorded yet.</p>
      <ul v-else style="list-style:none;padding:0;margin:0;display:grid;gap:4px;">
        <li v-for="item in rows" :key="item.deployment_uuid" style="font-family:var(--ui-font-mono);font-size:12px;">
          {{ item.deployment_uuid }}: {{ item.status }} ({{ item.created_at }})
        </li>
      </ul>
    </div>
  </div>
</template>
