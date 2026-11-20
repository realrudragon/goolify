<script setup lang="ts">
import { onMounted, ref, watch } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

interface S3Item {
  uuid: string;
  name: string;
  bucket: string;
  region: string;
  endpoint: string;
}

const props = defineProps<{ databaseUuid: string }>();
const emit = defineEmits<{ updated: [] }>();
const lifecycle = usePageLifecycle();

const frequency = ref('0 2 * * *');
const retentionCount = ref(7);
const result = ref('');
const s3Storages = ref<S3Item[]>([]);
const s3Name = ref('');
const s3Bucket = ref('');
const s3Region = ref('us-east-1');
const s3Endpoint = ref('');
const s3Key = ref('');
const s3Secret = ref('');

async function loadS3() {
  try {
    s3Storages.value = await apiRequest<S3Item[]>('/api/v1/s3-storages', { signal: lifecycle.signal });
  } catch {
    s3Storages.value = [];
  }
}

async function createSchedule() {
  if (!props.databaseUuid) return;
  result.value = '';
  try {
    const res = await apiRequest<{ uuid?: string; execution_uuid?: string }>(
      `/api/v1/databases/${encodeURIComponent(props.databaseUuid)}/backups`,
      {
        method: 'POST',
        signal: lifecycle.signal,
        body: JSON.stringify({
          frequency: frequency.value.trim() || '0 2 * * *',
          enabled: true,
          retention_count: retentionCount.value,
        }),
      },
    );
    result.value = `Backup schedule saved: ${res.uuid || res.execution_uuid}`;
    emit('updated');
  } catch (err) {
    result.value = (err as Error).message;
  }
}

async function createS3() {
  if (!s3Name.value.trim() || !s3Bucket.value.trim() || !s3Endpoint.value.trim()) return;
  result.value = '';
  try {
    await apiRequest('/api/v1/s3-storages', {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({
        name: s3Name.value.trim(),
        bucket: s3Bucket.value.trim(),
        region: s3Region.value.trim() || 'us-east-1',
        endpoint: s3Endpoint.value.trim(),
        key: s3Key.value,
        secret: s3Secret.value,
      }),
    });
    s3Name.value = '';
    s3Bucket.value = '';
    s3Endpoint.value = '';
    s3Key.value = '';
    s3Secret.value = '';
    result.value = 'S3 storage saved';
    await loadS3();
  } catch (err) {
    result.value = (err as Error).message;
  }
}

async function deleteS3(uuid: string) {
  try {
    await apiRequest(`/api/v1/s3-storages/${encodeURIComponent(uuid)}`, {
      method: 'DELETE',
      signal: lifecycle.signal,
    });
    await loadS3();
  } catch (err) {
    result.value = (err as Error).message;
  }
}

watch(() => props.databaseUuid, () => {
  result.value = '';
});

lifecycle.onTeamSwitch(() => {
  s3Storages.value = [];
  void loadS3();
});

onMounted(() => {
  void loadS3();
});
</script>

<template>
  <div class="coolify-card" data-testid="backup-manager">
    <div class="coolify-well" style="display:grid;gap:12px;">
      <strong>Backup Schedules &amp; S3 Storage Destinations</strong>

      <form style="display:flex;align-items:center;gap:8px;flex-wrap:wrap;" @submit.prevent="createSchedule">
        <input v-model="frequency" class="coolify-input" placeholder="Cron (0 2 * * *)" aria-label="Backup cron" />
        <input v-model.number="retentionCount" type="number" class="coolify-input" style="width:90px;" aria-label="Retention count" />
        <button type="submit" class="coolify-btn coolify-btn-primary" data-testid="create-backup-schedule-btn">Create Backup Schedule</button>
      </form>

      <form style="display:flex;gap:8px;flex-wrap:wrap;padding-top:8px;border-top:1px solid var(--ui-well-border);" @submit.prevent="createS3">
        <input v-model="s3Name" class="coolify-input" placeholder="S3 Name" required />
        <input v-model="s3Bucket" class="coolify-input" placeholder="Bucket" required />
        <input v-model="s3Region" class="coolify-input" style="width:110px;" placeholder="Region" />
        <input v-model="s3Endpoint" class="coolify-input" placeholder="Endpoint (https://...)" required />
        <input v-model="s3Key" class="coolify-input" placeholder="Access Key" />
        <input v-model="s3Secret" type="password" class="coolify-input" placeholder="Secret Key" />
        <button type="submit" class="coolify-btn">Add S3 Storage</button>
      </form>

      <p v-if="result" role="status" style="margin:0;">{{ result }}</p>

      <ul v-if="s3Storages.length > 0" style="list-style:none;padding:0;margin:0;display:grid;gap:4px;">
        <li v-for="s3 in s3Storages" :key="s3.uuid" style="display:flex;justify-content:space-between;align-items:center;">
          <span>{{ s3.name }} ({{ s3.bucket }} @ {{ s3.region }})</span>
          <button type="button" class="coolify-btn" @click="deleteS3(s3.uuid)">Delete</button>
        </li>
      </ul>
    </div>
  </div>
</template>
