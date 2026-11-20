<script setup lang="ts">
import { onMounted, ref } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

interface PrivateKeyDto {
  id: number;
  uuid: string;
  team_id: number;
}

const emit = defineEmits<{ created: [uuid: string] }>();
const lifecycle = usePageLifecycle();

const name = ref('');
const ip = ref('');
const port = ref(22);
const user = ref('root');
const privateKeyUuid = ref('');
const availableKeys = ref<PrivateKeyDto[]>([]);
const submitting = ref(false);
const message = ref('');

async function loadKeys() {
  try {
    availableKeys.value = await apiRequest<PrivateKeyDto[]>('/api/v1/security/keys', { signal: lifecycle.signal });
    if (!privateKeyUuid.value && availableKeys.value.length > 0) {
      privateKeyUuid.value = availableKeys.value[0].uuid;
    }
  } catch {
    availableKeys.value = [];
  }
}

async function submitServer() {
  submitting.value = true;
  message.value = '';
  try {
    const res = await apiRequest<{ uuid: string }>('/api/v1/servers', {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({
        name: name.value,
        ip: ip.value,
        port: port.value,
        user: user.value,
        private_key_uuid: privateKeyUuid.value,
      }),
    });
    message.value = 'Server registered';
    name.value = '';
    ip.value = '';
    emit('created', res.uuid);
  } catch (err) {
    message.value = (err as Error).message;
  } finally {
    submitting.value = false;
  }
}

lifecycle.onTeamSwitch(() => {
  privateKeyUuid.value = '';
  availableKeys.value = [];
  void loadKeys();
});

onMounted(() => {
  void loadKeys();
});
</script>

<template>
  <form class="coolify-card" @submit.prevent="submitServer">
    <div class="coolify-well" style="display:grid;gap:8px;">
      <strong>Add Server</strong>
      <input v-model="name" class="coolify-input" placeholder="Server Name" aria-label="Server Name" required />
      <input v-model="ip" class="coolify-input" placeholder="IP / Hostname" aria-label="IP / Hostname" required />
      <div style="display:flex;gap:8px;">
        <input v-model.number="port" type="number" class="coolify-input" placeholder="Port" aria-label="SSH Port" />
        <input v-model="user" class="coolify-input" placeholder="SSH User" aria-label="SSH User" />
      </div>
      <select v-if="availableKeys.length > 0" v-model="privateKeyUuid" class="coolify-input" aria-label="Private Key UUID">
        <option v-for="k in availableKeys" :key="k.uuid" :value="k.uuid">{{ k.uuid }}</option>
      </select>
      <input v-else v-model="privateKeyUuid" class="coolify-input" placeholder="Private Key UUID" aria-label="Private Key UUID" required />
      <button type="submit" class="coolify-btn coolify-btn-primary" :disabled="submitting">Save Server</button>
      <p v-if="message" role="status" style="margin:0;">{{ message }}</p>
    </div>
  </form>
</template>
