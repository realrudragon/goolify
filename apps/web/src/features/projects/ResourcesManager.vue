<script setup lang="ts">
import { onMounted, ref, watch } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

interface ResourceSummary {
  uuid: string;
  name: string;
  status?: string;
}

interface ProjectOption {
  uuid: string;
  name: string;
}

interface ServiceTemplateOption {
  type: string;
  name: string;
  slogan: string;
  category: string;
}

const emit = defineEmits<{
  selectApp: [uuid: string];
  selectService: [uuid: string];
}>();

const lifecycle = usePageLifecycle();
const apps = ref<ResourceSummary[]>([]);
const services = ref<ResourceSummary[]>([]);
const projects = ref<ProjectOption[]>([]);
const envs = ref<Array<{ uuid: string; name: string }>>([]);
const servers = ref<Array<{ uuid: string; name: string }>>([]);
const templates = ref<ServiceTemplateOption[]>([]);

const selectedProjectUuid = ref('');
const selectedEnvUuid = ref('');
const selectedServerUuid = ref('localhost');

const appName = ref('');
const appRepo = ref('https://github.com/coollabsio/coolify-examples');
const appBranch = ref('main');
const appType = ref<
  'public' | 'dockerfile' | 'dockerimage' | 'dockercompose' | 'static' | 'private-github-app' | 'private-deploy-key'
>('public');

const svcName = ref('');
const selectedTemplate = ref('');
const svcCompose = ref('services:\n  web:\n    image: nginx:alpine\n');
const status = ref('');

async function loadAll() {
  try {
    const [appList, svcList, projList, srvList, tplList] = await Promise.all([
      apiRequest<ResourceSummary[]>('/api/v1/applications', { signal: lifecycle.signal }).catch(() => []),
      apiRequest<ResourceSummary[]>('/api/v1/services', { signal: lifecycle.signal }).catch(() => []),
      apiRequest<ProjectOption[]>('/api/v1/projects', { signal: lifecycle.signal }).catch(() => []),
      apiRequest<Array<{ uuid: string; name: string }>>('/api/v1/servers', { signal: lifecycle.signal }).catch(() => []),
      apiRequest<ServiceTemplateOption[]>('/api/v1/services/templates', { signal: lifecycle.signal }).catch(() => []),
    ]);
    apps.value = appList;
    services.value = svcList;
    projects.value = projList;
    servers.value = srvList;
    templates.value = tplList;
    if (!selectedProjectUuid.value && projList.length > 0) {
      selectedProjectUuid.value = projList[0].uuid;
      await loadEnvs(projList[0].uuid);
    }
    if (srvList.length > 0 && !srvList.some((s) => s.uuid === selectedServerUuid.value)) {
      selectedServerUuid.value = srvList[0].uuid;
    }
  } catch {
    apps.value = [];
    services.value = [];
  }
}

async function loadEnvs(projUuid: string) {
  if (!projUuid) {
    envs.value = [];
    selectedEnvUuid.value = '';
    return;
  }
  try {
    envs.value = await apiRequest<Array<{ uuid: string; name: string }>>(
      `/api/v1/projects/${encodeURIComponent(projUuid)}/environments`,
      { signal: lifecycle.signal },
    );
    selectedEnvUuid.value = envs.value[0]?.uuid ?? '';
  } catch {
    envs.value = [];
    selectedEnvUuid.value = '';
  }
}

async function createApp() {
  if (!appName.value.trim() || !selectedProjectUuid.value || !selectedEnvUuid.value) {
    status.value = 'Select or create a project first.';
    return;
  }
  status.value = '';
  try {
    const created = await apiRequest<ResourceSummary>(
      `/api/v1/applications/${appType.value}`,
      {
        method: 'POST',
        signal: lifecycle.signal,
        body: JSON.stringify({
          name: appName.value.trim(),
          project_uuid: selectedProjectUuid.value,
          environment_uuid: selectedEnvUuid.value,
          server_uuid: selectedServerUuid.value,
          git_repository: appRepo.value.trim(),
          git_branch: appBranch.value.trim() || 'main',
          dockerfile: 'FROM alpine:3.20\nCMD ["sleep", "infinity"]\n',
          docker_registry_image_name: 'nginx',
          docker_registry_image_tag: 'alpine',
        }),
      },
    );
    appName.value = '';
    await loadAll();
    emit('selectApp', created.uuid);
    status.value = `Application created: ${created.name}`;
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function createService() {
  const effectiveName = svcName.value.trim() || selectedTemplate.value;
  if (!effectiveName || !selectedEnvUuid.value) {
    status.value = 'Select or create a project first.';
    return;
  }
  status.value = '';
  try {
    const payload: Record<string, string> = {
      name: effectiveName,
      project_uuid: selectedProjectUuid.value,
      environment_uuid: selectedEnvUuid.value,
      server_uuid: selectedServerUuid.value,
    };
    if (selectedTemplate.value) {
      payload.type = selectedTemplate.value;
    } else {
      payload.docker_compose_raw = svcCompose.value;
    }
    const created = await apiRequest<ResourceSummary>('/api/v1/services', {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify(payload),
    });
    svcName.value = '';
    selectedTemplate.value = '';
    await loadAll();
    emit('selectService', created.uuid);
    status.value = `Service created: ${created.name}`;
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function cloneApp(uuid: string) {
  status.value = '';
  try {
    const cloned = await apiRequest<ResourceSummary>(`/api/v1/applications/${encodeURIComponent(uuid)}/clone`, {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({}),
    });
    await loadAll();
    emit('selectApp', cloned.uuid);
    status.value = `Cloned application: ${cloned.name}`;
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function cloneService(uuid: string) {
  status.value = '';
  try {
    const cloned = await apiRequest<ResourceSummary>(`/api/v1/services/${encodeURIComponent(uuid)}/clone`, {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({}),
    });
    await loadAll();
    emit('selectService', cloned.uuid);
    status.value = `Cloned service: ${cloned.name}`;
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function cloneEnvironment() {
  if (!selectedProjectUuid.value || !selectedEnvUuid.value) return;
  status.value = '';
  try {
    const cloned = await apiRequest<{ uuid: string; name: string }>(
      `/api/v1/projects/${encodeURIComponent(selectedProjectUuid.value)}/environments/${encodeURIComponent(selectedEnvUuid.value)}/clone`,
      {
        method: 'POST',
        signal: lifecycle.signal,
        body: JSON.stringify({}),
      },
    );
    await loadEnvs(selectedProjectUuid.value);
    await loadAll();
    status.value = `Cloned environment: ${cloned.name}`;
  } catch (err) {
    status.value = (err as Error).message;
  }
}

watch(selectedProjectUuid, (u) => {
  void loadEnvs(u);
});

lifecycle.onTeamSwitch(() => {
  apps.value = [];
  services.value = [];
  status.value = '';
  void loadAll();
});

onMounted(() => {
  void loadAll();
});

defineExpose({ refresh: loadAll });
</script>

<template>
  <div class="coolify-card" data-testid="resources-manager">
    <div class="coolify-well" style="display:grid;gap:12px;">
      <div style="display:flex;justify-content:space-between;align-items:center;flex-wrap:wrap;gap:8px;">
        <strong>Applications ({{ apps.length }}) &amp; Services ({{ services.length }})</strong>
        <div style="display:flex;gap:8px;align-items:center;">
          <select v-model="selectedProjectUuid" class="coolify-input" aria-label="Project">
            <option v-for="p in projects" :key="p.uuid" :value="p.uuid">{{ p.name }}</option>
          </select>
          <select v-model="selectedEnvUuid" class="coolify-input" aria-label="Environment">
            <option v-for="e in envs" :key="e.uuid" :value="e.uuid">{{ e.name }}</option>
          </select>
          <select v-model="selectedServerUuid" class="coolify-input" aria-label="Server">
            <option v-for="s in servers" :key="s.uuid" :value="s.uuid">{{ s.name }}</option>
          </select>
          <button
            v-if="selectedProjectUuid && selectedEnvUuid"
            type="button"
            class="coolify-btn"
            data-testid="clone-env-btn"
            @click="cloneEnvironment"
          >
            Clone Env
          </button>
          <button type="button" class="coolify-btn" @click="loadAll">Refresh</button>
        </div>
      </div>

      <form style="display:flex;gap:8px;flex-wrap:wrap;" @submit.prevent="createApp">
        <input v-model="appName" class="coolify-input" placeholder="New application name" data-testid="new-app-name" required />
        <select v-model="appType" class="coolify-input" aria-label="Application type">
          <option value="public">Public Git (Railpack)</option>
          <option value="dockerfile">Dockerfile</option>
          <option value="dockerimage">Docker Image</option>
          <option value="dockercompose">Docker Compose</option>
          <option value="static">Static Site</option>
          <option value="private-github-app">GitHub App</option>
          <option value="private-deploy-key">Private SSH Deploy Key</option>
        </select>
        <input v-model="appRepo" class="coolify-input" placeholder="Git repository URL" />
        <input v-model="appBranch" class="coolify-input" style="width:90px;" placeholder="Branch" />
        <button type="submit" class="coolify-btn coolify-btn-primary" data-testid="create-app-btn">Create Application</button>
      </form>

      <form style="display:flex;gap:8px;flex-wrap:wrap;" @submit.prevent="createService">
        <select v-model="selectedTemplate" class="coolify-input" aria-label="One-click service template" data-testid="service-template-select">
          <option value="">Custom Compose YAML</option>
          <option v-for="tpl in templates" :key="tpl.type" :value="tpl.type">
            {{ tpl.name }} ({{ tpl.category }})
          </option>
        </select>
        <input v-model="svcName" class="coolify-input" placeholder="New service name" data-testid="new-svc-name" :required="!selectedTemplate" />
        <button type="submit" class="coolify-btn" data-testid="create-svc-btn">Create Compose Service</button>
      </form>

      <p v-if="status" role="status" style="margin:0;">{{ status }}</p>

      <div style="display:grid;grid-template-columns:1fr 1fr;gap:10px;">
        <div>
          <strong style="font-size:13px;">Applications</strong>
          <ul style="list-style:none;padding:0;margin:6px 0 0;display:grid;gap:4px;">
            <li v-for="a in apps" :key="a.uuid" style="display:flex;gap:4px;align-items:center;">
              <button type="button" class="coolify-btn" style="flex:1;text-align:left;" @click="emit('selectApp', a.uuid)">
                {{ a.name }} ({{ a.status || 'stopped' }})
              </button>
              <button type="button" class="coolify-btn" @click="cloneApp(a.uuid)">Clone</button>
            </li>
          </ul>
        </div>
        <div>
          <strong style="font-size:13px;">Services</strong>
          <ul style="list-style:none;padding:0;margin:6px 0 0;display:grid;gap:4px;">
            <li v-for="sv in services" :key="sv.uuid" style="display:flex;gap:4px;align-items:center;">
              <button type="button" class="coolify-btn" style="flex:1;text-align:left;" @click="emit('selectService', sv.uuid)">
                {{ sv.name }} ({{ sv.status || 'stopped' }})
              </button>
              <button type="button" class="coolify-btn" @click="cloneService(sv.uuid)">Clone</button>
            </li>
          </ul>
        </div>
      </div>
    </div>
  </div>
</template>
