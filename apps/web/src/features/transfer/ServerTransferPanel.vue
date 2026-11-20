<script setup lang="ts">
import { ref } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const props = defineProps<{ serverUuid: string }>();
const lifecycle = usePageLifecycle();
const targetUrl = ref('');
const markArchived = ref(false);
const bundleStatus = ref('');
const checklist = ref<Array<{ type: string; message: string }>>([]);

async function startExport() {
  bundleStatus.value = '';
  checklist.value = [];
  try {
    const res = await apiRequest<{
      bundle_version?: string;
      post_transfer_checklist?: Array<{ type: string; message: string }>;
    }>(`/api/v1/servers/${encodeURIComponent(props.serverUuid)}/transfer`, {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({
        target_url: targetUrl.value,
        mark_transferred_away: markArchived.value,
      }),
    });
    bundleStatus.value = `Transfer bundle generated (${res.bundle_version ?? 'v1'})`;
    checklist.value = res.post_transfer_checklist ?? [];
  } catch (err) {
    bundleStatus.value = (err as Error).message;
  }
}
</script>

<template>
  <form class="coolify-card" @submit.prevent="startExport">
    <div class="coolify-well" style="display:grid;gap:8px;">
      <strong>Server Transfer</strong>
      <input v-model="targetUrl" class="coolify-input" placeholder="Target Goolify or Coolify URL" required />
      <label><input v-model="markArchived" type="checkbox" /> Mark source server as transferred archive</label>
      <button type="submit" class="coolify-btn coolify-btn-primary">Export &amp; Transfer</button>
      <p v-if="bundleStatus" role="status" style="margin:0;">{{ bundleStatus }}</p>
      <ul v-if="checklist.length > 0" style="margin:0;padding-left:18px;">
        <li v-for="(item, idx) in checklist" :key="idx">{{ item.message }}</li>
      </ul>
    </div>
  </form>
</template>
