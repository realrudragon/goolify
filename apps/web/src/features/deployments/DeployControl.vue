<script setup lang="ts">
import { onMounted, ref, watch } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const props = defineProps<{ applicationUuid: string }>();
const emit = defineEmits<{ deployed: [] }>();
const lifecycle = usePageLifecycle();
const status = ref<string>('idle');
const forceRebuild = ref(false);
const envKey = ref('');
const envVal = ref('');
const envs = ref<Array<{ uuid: string; key: string; value?: string }>>([]);
const rollbackImages = ref<Array<{ tag?: string; image?: string; sha?: string }>>([]);

async function loadDetails() {
  if (!props.applicationUuid) return;
  try {
    const [envList, rbList] = await Promise.all([
      apiRequest<Array<{ uuid: string; key: string; value?: string }>>(
        `/api/v1/applications/${encodeURIComponent(props.applicationUuid)}/envs`,
        { signal: lifecycle.signal },
      ).catch(() => []),
      apiRequest<Array<{ tag?: string; image?: string; sha?: string }>>(
        `/api/v1/applications/${encodeURIComponent(props.applicationUuid)}/rollback-images`,
        { signal: lifecycle.signal },
      ).catch(() => []),
    ]);
    envs.value = envList;
    rollbackImages.value = rbList;
  } catch {
    envs.value = [];
  }
}

async function triggerDeploy() {
  status.value = 'queueing';
  try {
    const res = await apiRequest<{ deployment_uuid?: string; status?: string; deployments?: Array<{ deployment_uuid: string }> }>(
      `/api/v1/deploy?uuid=${encodeURIComponent(props.applicationUuid)}&force=${forceRebuild.value}`,
      { method: 'POST', signal: lifecycle.signal },
    );
    const depUuid = res.deployment_uuid || res.deployments?.[0]?.deployment_uuid || 'queued';
    status.value = `${res.status || 'queued'} (${depUuid})`;
    emit('deployed');
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function lifecycleAction(act: 'start' | 'restart' | 'stop') {
  status.value = '';
  try {
    await apiRequest(`/api/v1/applications/${encodeURIComponent(props.applicationUuid)}/${act}`, {
      method: 'POST',
      signal: lifecycle.signal,
    });
    status.value = `Application ${act} queued`;
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function addEnv() {
  if (!envKey.value.trim()) return;
  try {
    await apiRequest(`/api/v1/applications/${encodeURIComponent(props.applicationUuid)}/envs`, {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({ key: envKey.value.trim(), value: envVal.value }),
    });
    envKey.value = '';
    envVal.value = '';
    status.value = 'Environment variable saved';
    await loadDetails();
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function deleteEnv(uuid: string) {
  try {
    await apiRequest(`/api/v1/applications/${encodeURIComponent(props.applicationUuid)}/envs/${encodeURIComponent(uuid)}`, {
      method: 'DELETE',
      signal: lifecycle.signal,
    });
    await loadDetails();
  } catch (err) {
    status.value = (err as Error).message;
  }
}

watch(() => props.applicationUuid, () => {
  void loadDetails();
});

onMounted(() => {
  void loadDetails();
});
</script>

<template>
  <div class="coolify-card">
    <div class="coolify-well" style="display:grid;gap:10px;">
      <div style="display:flex;align-items:center;gap:8px;flex-wrap:wrap;">
        <button type="button" class="coolify-btn coolify-btn-primary" @click="triggerDeploy">Deploy</button>
        <button type="button" class="coolify-btn" @click="lifecycleAction('start')">Start</button>
        <button type="button" class="coolify-btn" @click="lifecycleAction('restart')">Restart</button>
        <button type="button" class="coolify-btn" @click="lifecycleAction('stop')">Stop</button>
        <label><input v-model="forceRebuild" type="checkbox" /> Force rebuild</label>
        <span>State: {{ status }}</span>
      </div>

      <form style="display:flex;gap:8px;flex-wrap:wrap;" @submit.prevent="addEnv">
        <input v-model="envKey" class="coolify-input" placeholder="ENV_KEY" required />
        <input v-model="envVal" class="coolify-input" placeholder="Value" />
        <button type="submit" class="coolify-btn">Add Env Var</button>
      </form>

      <ul v-if="envs.length > 0" style="list-style:none;padding:0;margin:0;display:grid;gap:4px;">
        <li v-for="ev in envs" :key="ev.uuid" style="display:flex;justify-content:space-between;align-items:center;font-family:var(--ui-font-mono);font-size:12px;">
          <span>{{ ev.key }}</span>
          <button type="button" class="coolify-btn" @click="deleteEnv(ev.uuid)">Delete</button>
        </li>
      </ul>
      <div v-if="rollbackImages.length > 0" style="font-size:12px;color:var(--ui-fg-dim);">
        Rollback images available: {{ rollbackImages.length }}
      </div>
    </div>
  </div>
</template>
