<script setup lang="ts">
import { onMounted, ref } from 'vue';
import SettingsSection from '../../components/SettingsSection.vue';
import { apiRequest } from '../../lib/api';
import { pushToast } from '../../lib/toast';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const props = withDefaults(defineProps<{ serverUuid?: string }>(), { serverUuid: 'localhost' });
const lifecycle = usePageLifecycle();
const proxyType = ref<'traefik' | 'caddy'>('traefik');
const proxyStatus = ref('running');
const composeYaml = ref('');
const status = ref('');
const error = ref('');
const busy = ref(false);

async function loadProxy() {
  const token = lifecycle.beginLoad();
  error.value = '';
  try {
    const res = await apiRequest<{
      proxy_type?: 'traefik' | 'caddy';
      status?: string;
      compose_yaml?: string;
      main_yaml?: string;
    }>(
      `/api/v1/servers/${encodeURIComponent(props.serverUuid)}/proxy?type=${proxyType.value}`,
      { signal: token.signal },
    );
    if (!token.isCurrent()) return;
    if (res.proxy_type) proxyType.value = res.proxy_type;
    proxyStatus.value = res.status ?? 'running';
    composeYaml.value = res.compose_yaml ?? res.main_yaml ?? '';
    status.value = `Loaded ${proxyType.value} configuration`;
  } catch (err) {
    if (!token.isCurrent()) return;
    error.value = (err as Error).message;
  }
}

async function saveConfiguration() {
  if (busy.value || !composeYaml.value.trim()) return;
  busy.value = true;
  error.value = '';
  try {
    await apiRequest(`/api/v1/servers/${encodeURIComponent(props.serverUuid)}/proxy`, {
      method: 'PATCH',
      body: JSON.stringify({ type: proxyType.value }),
    });
    await apiRequest(`/api/v1/servers/${encodeURIComponent(props.serverUuid)}/proxy/configuration`, {
      method: 'PUT',
      body: JSON.stringify({ configuration: composeYaml.value }),
    });
    status.value = 'Proxy configuration saved.';
    pushToast('Proxy configuration saved.', 'success');
    await loadProxy();
  } catch (err) {
    error.value = (err as Error).message;
    pushToast(error.value, 'error');
  } finally {
    busy.value = false;
  }
}

async function resetDefaultConfig() {
  if (busy.value) return;
  busy.value = true;
  error.value = '';
  try {
    await apiRequest(`/api/v1/servers/${encodeURIComponent(props.serverUuid)}/proxy`, {
      method: 'PATCH',
      body: JSON.stringify({ type: proxyType.value, custom_configuration: '' }),
    });
    await loadProxy();
    pushToast('Reset to default proxy configuration.', 'info');
  } catch (err) {
    error.value = (err as Error).message;
  } finally {
    busy.value = false;
  }
}

async function proxyAction(action: 'start' | 'restart' | 'stop') {
  if (busy.value) return;
  busy.value = true;
  error.value = '';
  try {
    const res = await apiRequest<{ message?: string }>(
      `/api/v1/servers/${encodeURIComponent(props.serverUuid)}/proxy/${action}`,
      { method: 'POST', body: '{}' },
    );
    status.value = res.message ?? `Proxy ${action} queued.`;
    pushToast(status.value, 'success');
    await loadProxy();
  } catch (err) {
    error.value = (err as Error).message;
    pushToast(error.value, 'error');
  } finally {
    busy.value = false;
  }
}

onMounted(loadProxy);
</script>

<template>
  <SettingsSection
    title="Proxy"
    helper="Manage Traefik or Caddy reverse proxy configuration and lifecycle on this server."
  >
    <template #actions>
      <span class="status-badge" :class="{ warning: proxyStatus !== 'running' }">
        {{ proxyStatus }}
      </span>
      <button type="button" class="coolify-btn" :disabled="busy" @click="proxyAction('start')">
        Start Proxy
      </button>
      <button type="button" class="coolify-btn coolify-btn-primary" :disabled="busy" @click="proxyAction('restart')">
        Restart Proxy
      </button>
      <button type="button" class="coolify-btn danger-button" :disabled="busy" @click="proxyAction('stop')">
        Stop Proxy
      </button>
    </template>

    <p v-if="error" role="alert" class="page-error">{{ error }}</p>
    <p v-if="status" role="status" class="page-muted">{{ status }}</p>

    <div class="form-grid">
      <label class="form-field">
        Proxy Engine
        <select v-model="proxyType" class="coolify-input" aria-label="Proxy Type" @change="loadProxy">
          <option value="traefik">Traefik v3</option>
          <option value="caddy">Caddy</option>
        </select>
      </label>
      <div class="page-actions" style="align-items: end;">
        <button type="button" class="coolify-btn" :disabled="busy" @click="loadProxy">
          Inspect Config
        </button>
        <button type="button" class="coolify-btn" :disabled="busy" @click="resetDefaultConfig">
          Reset Default
        </button>
        <button type="button" class="coolify-btn coolify-btn-primary" :disabled="busy" @click="saveConfiguration">
          Save Configuration
        </button>
      </div>
      <label class="form-field full-span">
        Proxy Docker Compose Configuration (/data/coolify/proxy/docker-compose.yml)
        <textarea
          v-model="composeYaml"
          class="coolify-input code-editor"
          rows="16"
          aria-label="Proxy configuration"
        />
      </label>
    </div>
  </SettingsSection>
</template>
