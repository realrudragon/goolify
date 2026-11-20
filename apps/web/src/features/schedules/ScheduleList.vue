<script setup lang="ts">
import { onMounted, ref, watch } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

export interface ScheduleItem {
  id: number;
  uuid?: string;
  name: string;
  cron: string;
  enabled: boolean;
  kind: 'database_backup' | 'application_task' | 'service_task';
  resourceUuid: string;
}

const props = withDefaults(
  defineProps<{
    databaseUuid?: string;
    applicationUuid?: string;
    serviceUuid?: string;
  }>(),
  {
    databaseUuid: '',
    applicationUuid: '',
    serviceUuid: '',
  },
);

const lifecycle = usePageLifecycle();
const items = ref<ScheduleItem[]>([]);
const taskName = ref('');
const taskCommand = ref('');
const taskFrequency = ref('0 * * * *');
const status = ref('');

async function loadSchedules() {
  const loaded: ScheduleItem[] = [];
  try {
    if (props.databaseUuid) {
      const dbBackups = await apiRequest<Array<{ id: number; uuid: string; frequency?: string; enabled?: boolean; status?: string }>>(
        `/api/v1/databases/${encodeURIComponent(props.databaseUuid)}/backups`,
        { signal: lifecycle.signal },
      ).catch(() => []);
      for (const b of dbBackups) {
        loaded.push({
          id: b.id,
          uuid: b.uuid,
          name: `DB Backup (${props.databaseUuid})`,
          cron: b.frequency || 'manual/scheduled',
          enabled: b.enabled ?? true,
          kind: 'database_backup',
          resourceUuid: props.databaseUuid,
        });
      }
    }
    if (props.applicationUuid) {
      const appTasks = await apiRequest<Array<{ id: number; uuid: string; name: string; frequency: string; enabled: boolean }>>(
        `/api/v1/applications/${encodeURIComponent(props.applicationUuid)}/scheduled-tasks`,
        { signal: lifecycle.signal },
      ).catch(() => []);
      for (const t of appTasks) {
        loaded.push({
          id: t.id,
          uuid: t.uuid,
          name: t.name,
          cron: t.frequency,
          enabled: t.enabled,
          kind: 'application_task',
          resourceUuid: props.applicationUuid,
        });
      }
    }
    if (props.serviceUuid) {
      const svcTasks = await apiRequest<Array<{ id: number; uuid: string; name: string; frequency: string; enabled: boolean }>>(
        `/api/v1/services/${encodeURIComponent(props.serviceUuid)}/scheduled-tasks`,
        { signal: lifecycle.signal },
      ).catch(() => []);
      for (const t of svcTasks) {
        loaded.push({
          id: t.id,
          uuid: t.uuid,
          name: t.name,
          cron: t.frequency,
          enabled: t.enabled,
          kind: 'service_task',
          resourceUuid: props.serviceUuid,
        });
      }
    }
    items.value = loaded;
  } catch {
    items.value = [];
  }
}

async function createTask() {
  const targetPath = props.applicationUuid
    ? `/api/v1/applications/${encodeURIComponent(props.applicationUuid)}/scheduled-tasks`
    : props.serviceUuid
      ? `/api/v1/services/${encodeURIComponent(props.serviceUuid)}/scheduled-tasks`
      : '';
  if (!targetPath || !taskName.value.trim() || !taskCommand.value.trim()) return;
  status.value = '';
  try {
    await apiRequest(targetPath, {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({
        name: taskName.value.trim(),
        command: taskCommand.value.trim(),
        frequency: taskFrequency.value.trim() || '0 * * * *',
        enabled: true,
      }),
    });
    taskName.value = '';
    taskCommand.value = '';
    status.value = 'Scheduled task saved';
    await loadSchedules();
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function runTaskNow(item: ScheduleItem) {
  if (!item.uuid || item.kind === 'database_backup') return;
  const base = item.kind === 'application_task' ? 'applications' : 'services';
  status.value = '';
  try {
    await apiRequest(`/api/v1/${base}/${encodeURIComponent(item.resourceUuid)}/scheduled-tasks/${encodeURIComponent(item.uuid)}/execute`, {
      method: 'POST',
      signal: lifecycle.signal,
    });
    status.value = `Queued execution for ${item.name}`;
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function deleteItem(item: ScheduleItem) {
  if (!item.uuid) return;
  status.value = '';
  try {
    if (item.kind === 'database_backup') {
      await apiRequest(`/api/v1/databases/${encodeURIComponent(item.resourceUuid)}/backups/${encodeURIComponent(item.uuid)}`, {
        method: 'DELETE',
        signal: lifecycle.signal,
      });
    } else {
      const base = item.kind === 'application_task' ? 'applications' : 'services';
      await apiRequest(`/api/v1/${base}/${encodeURIComponent(item.resourceUuid)}/scheduled-tasks/${encodeURIComponent(item.uuid)}`, {
        method: 'DELETE',
        signal: lifecycle.signal,
      });
    }
    status.value = `Removed ${item.name}`;
    await loadSchedules();
  } catch (err) {
    status.value = (err as Error).message;
  }
}

watch(() => [props.databaseUuid, props.applicationUuid, props.serviceUuid], () => {
  void loadSchedules();
});

lifecycle.onTeamSwitch(() => {
  items.value = [];
  status.value = '';
  void loadSchedules();
});

onMounted(() => {
  void loadSchedules();
});

defineExpose({ refresh: loadSchedules });
</script>

<template>
  <div class="coolify-card" data-testid="schedule-list">
    <div class="coolify-well" style="display:grid;gap:10px;">
      <div style="display:flex;justify-content:space-between;align-items:center;">
        <strong>Scheduled Tasks &amp; Backups</strong>
        <button type="button" class="coolify-btn" @click="loadSchedules">Refresh</button>
      </div>
      <form
        v-if="props.applicationUuid || props.serviceUuid"
        style="display:flex;gap:8px;flex-wrap:wrap;"
        @submit.prevent="createTask"
      >
        <input v-model="taskName" class="coolify-input" placeholder="Task name" required />
        <input v-model="taskCommand" class="coolify-input" placeholder="Command (e.g. php artisan schedule:run)" required />
        <input v-model="taskFrequency" class="coolify-input" placeholder="Cron (0 * * * *)" />
        <button type="submit" class="coolify-btn coolify-btn-primary">Add Scheduled Task</button>
      </form>
      <p v-if="status" role="status" style="margin:0;">{{ status }}</p>
      <p v-if="items.length === 0" style="color:var(--ui-fg-dim);margin:0;">No schedules persisted for the selected resource.</p>
      <ul v-else style="list-style:none;padding:0;margin:0;display:grid;gap:6px;">
        <li
          v-for="item in items"
          :key="`${item.kind}-${item.uuid || item.id}`"
          style="display:flex;justify-content:space-between;align-items:center;gap:8px;padding:6px 8px;border:1px solid var(--ui-well-border);border-radius:6px;"
        >
          <span>{{ item.name }} ({{ item.cron }}): {{ item.enabled ? 'Enabled' : 'Paused' }}</span>
          <span style="display:flex;gap:6px;">
            <button v-if="item.kind !== 'database_backup' && item.uuid" type="button" class="coolify-btn" @click="runTaskNow(item)">Run Now</button>
            <button v-if="item.uuid" type="button" class="coolify-btn" @click="deleteItem(item)">Delete</button>
          </span>
        </li>
      </ul>
    </div>
  </div>
</template>
