<script setup lang="ts">
import { onMounted, ref, watch } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

interface DatabaseItem {
  id: number;
  uuid: string;
  name: string;
  type: string;
  status: string;
  public_port?: number | null;
  is_public?: boolean;
}

interface ProjectWithEnvs {
  uuid: string;
  name: string;
  environments?: Array<{ uuid: string; name: string }>;
}

const props = defineProps<{ databaseUuid: string; dbType: string }>();
const emit = defineEmits<{ select: [uuid: string, dbType: string] }>();
const lifecycle = usePageLifecycle();

const databases = ref<DatabaseItem[]>([]);
const projects = ref<ProjectWithEnvs[]>([]);
const servers = ref<Array<{ uuid: string; name: string }>>([]);

const newName = ref('');
const newType = ref('postgresql');
const selectedProjectUuid = ref('');
const envs = ref<Array<{ uuid: string; name: string }>>([]);
const selectedEnvUuid = ref('');
const selectedServerUuid = ref('localhost');

const publicPort = ref<number>(5432);
const isPublic = ref(false);
const status = ref('');

async function loadAll() {
  const token = lifecycle.beginLoad();
  try {
    const [dbList, projList, srvList] = await Promise.all([
      apiRequest<DatabaseItem[]>('/api/v1/databases', { signal: token.signal }).catch(() => []),
      apiRequest<ProjectWithEnvs[]>('/api/v1/projects', { signal: token.signal }).catch(() => []),
      apiRequest<Array<{ uuid: string; name: string }>>('/api/v1/servers', { signal: token.signal }).catch(() => []),
    ]);
    if (!token.isCurrent()) return;
    databases.value = dbList;
    projects.value = projList;
    servers.value = srvList;
    if (!selectedProjectUuid.value && projList.length > 0) {
      selectedProjectUuid.value = projList[0].uuid;
      await loadProjectEnvs(projList[0].uuid);
    }
    if (!token.isCurrent()) return;
    if (srvList.length > 0 && !srvList.some((s) => s.uuid === selectedServerUuid.value)) {
      selectedServerUuid.value = srvList[0].uuid;
    }
    const cur = dbList.find((d) => d.uuid === props.databaseUuid);
    if (cur) {
      publicPort.value = cur.public_port ?? 5432;
      isPublic.value = Boolean(cur.is_public);
    }
  } catch {
    if (!token.isCurrent()) return;
    databases.value = [];
  }
}

async function loadProjectEnvs(projUuid: string) {
  if (!projUuid) {
    envs.value = [];
    selectedEnvUuid.value = '';
    return;
  }
  try {
    const envList = await apiRequest<Array<{ uuid: string; name: string }>>(
      `/api/v1/projects/${encodeURIComponent(projUuid)}/environments`,
      { signal: lifecycle.signal },
    );
    envs.value = envList;
    const p = projects.value.find((item) => item.uuid === projUuid);
    if (p) p.environments = envList;
    selectedEnvUuid.value = envList[0]?.uuid ?? '';
  } catch {
    envs.value = [];
    selectedEnvUuid.value = '';
  }
}

async function createDatabase() {
  if (!newName.value.trim()) return;
  if (selectedProjectUuid.value && !selectedEnvUuid.value) {
    await loadProjectEnvs(selectedProjectUuid.value);
  }
  if (!selectedProjectUuid.value || !selectedEnvUuid.value) {
    status.value = 'Create a project first so an environment is available.';
    return;
  }
  status.value = '';
  try {
    const created = await apiRequest<DatabaseItem>(
      `/api/v1/databases/${encodeURIComponent(newType.value)}`,
      {
        method: 'POST',
        signal: lifecycle.signal,
        body: JSON.stringify({
          name: newName.value.trim(),
          project_uuid: selectedProjectUuid.value,
          environment_uuid: selectedEnvUuid.value,
          server_uuid: selectedServerUuid.value,
        }),
      },
    );
    newName.value = '';
    await loadAll();
    emit('select', created.uuid, created.type);
    status.value = `Created ${created.type} database: ${created.name}`;
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function saveDatabase() {
  if (!props.databaseUuid) return;
  status.value = '';
  try {
    await apiRequest(`/api/v1/databases/${encodeURIComponent(props.databaseUuid)}`, {
      method: 'PATCH',
      signal: lifecycle.signal,
      body: JSON.stringify({
        is_public: isPublic.value,
        public_port: publicPort.value,
      }),
    });
    status.value = `Updated ${props.dbType} configuration`;
    await loadAll();
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function dbLifecycle(uuid: string, action: 'start' | 'restart' | 'stop') {
  status.value = '';
  try {
    await apiRequest(`/api/v1/databases/${encodeURIComponent(uuid)}/${action}`, {
      method: 'POST',
      signal: lifecycle.signal,
    });
    status.value = `Queued ${action} for ${uuid}`;
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function cloneDatabase(uuid: string) {
  status.value = '';
  try {
    const created = await apiRequest<DatabaseItem>(`/api/v1/databases/${encodeURIComponent(uuid)}/clone`, {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({}),
    });
    await loadAll();
    emit('select', created.uuid, created.type);
    status.value = `Cloned database: ${created.name}`;
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function deleteDatabase(uuid: string) {
  status.value = '';
  try {
    await apiRequest(`/api/v1/databases/${encodeURIComponent(uuid)}`, {
      method: 'DELETE',
      signal: lifecycle.signal,
    });
    status.value = `Deleted database ${uuid}`;
    await loadAll();
  } catch (err) {
    status.value = (err as Error).message;
  }
}

watch(selectedProjectUuid, (uuid) => {
  void loadProjectEnvs(uuid);
});

watch(() => props.databaseUuid, (uuid) => {
  const cur = databases.value.find((d) => d.uuid === uuid);
  if (cur) {
    publicPort.value = cur.public_port ?? 5432;
    isPublic.value = Boolean(cur.is_public);
  }
});

lifecycle.onTeamSwitch(() => {
  databases.value = [];
  status.value = '';
  void loadAll();
});

onMounted(() => {
  void loadAll();
});

defineExpose({ refresh: loadAll });
</script>

<template>
  <div class="coolify-card" data-testid="database-panel">
    <div class="coolify-well" style="display:grid;gap:12px;">
      <div style="display:flex;justify-content:space-between;align-items:center;">
        <strong>Standalone Databases ({{ databases.length }})</strong>
        <button type="button" class="coolify-btn" @click="loadAll">Refresh</button>
      </div>

      <form style="display:flex;gap:8px;flex-wrap:wrap;align-items:center;" @submit.prevent="createDatabase">
        <input v-model="newName" class="coolify-input" placeholder="Database name" data-testid="db-name-input" required />
        <select v-model="newType" class="coolify-input" aria-label="Database engine" data-testid="db-type-select">
          <option value="postgresql">PostgreSQL</option>
          <option value="mysql">MySQL</option>
          <option value="mariadb">MariaDB</option>
          <option value="mongodb">MongoDB</option>
          <option value="redis">Redis</option>
          <option value="clickhouse">ClickHouse</option>
          <option value="dragonfly">Dragonfly</option>
          <option value="keydb">KeyDB</option>
          <option value="sqlite">SQLite</option>
        </select>
        <select v-model="selectedProjectUuid" class="coolify-input" aria-label="Target project">
          <option v-for="p in projects" :key="p.uuid" :value="p.uuid">{{ p.name }}</option>
        </select>
        <select v-model="selectedEnvUuid" class="coolify-input" aria-label="Target environment">
          <option v-for="e in envs" :key="e.uuid" :value="e.uuid">{{ e.name }}</option>
        </select>
        <select v-model="selectedServerUuid" class="coolify-input" aria-label="Target server">
          <option v-for="s in servers" :key="s.uuid" :value="s.uuid">{{ s.name }}</option>
        </select>
        <button type="submit" class="coolify-btn coolify-btn-primary" data-testid="db-create-btn">Create Database</button>
      </form>

      <p v-if="status" role="status" style="margin:0;">{{ status }}</p>

      <ul style="list-style:none;padding:0;margin:0;display:grid;gap:6px;">
        <li
          v-for="db in databases"
          :key="db.uuid"
          style="display:flex;justify-content:space-between;align-items:center;gap:8px;padding:6px 8px;border:1px solid var(--ui-well-border);border-radius:6px;flex-wrap:wrap;"
        >
          <button type="button" class="coolify-btn" @click="emit('select', db.uuid, db.type)">
            {{ db.name }} ({{ db.type }}: {{ db.status }})
          </button>
          <span style="display:flex;gap:6px;">
            <button type="button" class="coolify-btn" @click="dbLifecycle(db.uuid, 'start')">Start</button>
            <button type="button" class="coolify-btn" @click="dbLifecycle(db.uuid, 'restart')">Restart</button>
            <button type="button" class="coolify-btn" @click="dbLifecycle(db.uuid, 'stop')">Stop</button>
            <button type="button" class="coolify-btn" @click="cloneDatabase(db.uuid)">Clone</button>
            <button v-if="db.id !== 0" type="button" class="coolify-btn" @click="deleteDatabase(db.uuid)">Delete</button>
          </span>
        </li>
      </ul>

      <form v-if="databaseUuid" style="display:flex;gap:8px;align-items:center;flex-wrap:wrap;padding-top:6px;border-top:1px solid var(--ui-well-border);" @submit.prevent="saveDatabase">
        <strong>Selected: {{ databaseUuid }} ({{ dbType }})</strong>
        <label><input v-model="isPublic" type="checkbox" /> Public access</label>
        <input v-model.number="publicPort" type="number" class="coolify-input" style="width:110px;" aria-label="Public port" />
        <button type="submit" class="coolify-btn coolify-btn-primary">Save Settings</button>
      </form>
    </div>
  </div>
</template>
