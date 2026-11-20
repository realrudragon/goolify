<script setup lang="ts">
import { onMounted, ref, watch } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const props = defineProps<{ serviceUuid: string }>();
const lifecycle = usePageLifecycle();
const composeYaml = ref('');
const serviceName = ref('');
const status = ref('');

async function loadService() {
  if (!props.serviceUuid) return;
  try {
    const svc = await apiRequest<{ name?: string; docker_compose_raw?: string }>(
      `/api/v1/services/${encodeURIComponent(props.serviceUuid)}`,
      { signal: lifecycle.signal },
    );
    serviceName.value = svc.name ?? props.serviceUuid;
    composeYaml.value = svc.docker_compose_raw ?? '';
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function saveAndParse() {
  status.value = '';
  try {
    await apiRequest(`/api/v1/services/${encodeURIComponent(props.serviceUuid)}`, {
      method: 'PATCH',
      signal: lifecycle.signal,
      body: JSON.stringify({ docker_compose_raw: composeYaml.value }),
    });
    status.value = 'Compose configuration saved';
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function serviceAction(action: 'start' | 'restart' | 'stop') {
  status.value = '';
  try {
    await apiRequest(`/api/v1/services/${encodeURIComponent(props.serviceUuid)}/${action}`, {
      method: 'POST',
      signal: lifecycle.signal,
    });
    status.value = `Service ${action} queued`;
  } catch (err) {
    status.value = (err as Error).message;
  }
}

watch(() => props.serviceUuid, () => {
  void loadService();
});

onMounted(() => {
  void loadService();
});
</script>

<template>
  <form class="coolify-card" @submit.prevent="saveAndParse">
    <div class="coolify-well" style="display:grid;gap:8px;">
      <div style="display:flex;justify-content:space-between;align-items:center;gap:8px;flex-wrap:wrap;">
        <strong>Compose &amp; Service Editor ({{ serviceName || serviceUuid }})</strong>
        <div style="display:flex;gap:6px;">
          <button type="button" class="coolify-btn" @click="serviceAction('start')">Start</button>
          <button type="button" class="coolify-btn" @click="serviceAction('restart')">Restart</button>
          <button type="button" class="coolify-btn" @click="serviceAction('stop')">Stop</button>
        </div>
      </div>
      <textarea v-model="composeYaml" rows="6" class="coolify-input" style="height:auto;padding:8px;font-family:var(--ui-font-mono);"></textarea>
      <button type="submit" class="coolify-btn coolify-btn-primary">Save Compose</button>
      <p v-if="status" role="status" style="margin:0;">{{ status }}</p>
    </div>
  </form>
</template>
