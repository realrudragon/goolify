<script setup lang="ts">
import { onMounted, ref } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

interface TokenRow {
  id: number;
  name: string;
  abilities?: string[];
  created_at?: string;
}

const lifecycle = usePageLifecycle();
const tokens = ref<TokenRow[]>([]);
const newName = ref('');
const ability = ref<'read' | 'write' | 'deploy' | 'root'>('read');
const issuedPlain = ref('');
const status = ref('');

async function loadTokens() {
  try {
    tokens.value = await apiRequest<TokenRow[]>('/app/auth/tokens', { signal: lifecycle.signal });
  } catch {
    tokens.value = [];
  }
}

async function createToken() {
  status.value = '';
  issuedPlain.value = '';
  try {
    const res = await apiRequest<{ id: number; token: string }>('/app/auth/tokens', {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({ name: newName.value.trim(), abilities: [ability.value] }),
    });
    issuedPlain.value = res.token;
    newName.value = '';
    status.value = 'API token created (copy now, shown once)';
    await loadTokens();
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function revokeToken(id: number) {
  status.value = '';
  try {
    await apiRequest(`/app/auth/tokens/${id}`, { method: 'DELETE', signal: lifecycle.signal });
    status.value = 'API token revoked';
    await loadTokens();
  } catch (err) {
    status.value = (err as Error).message;
  }
}

lifecycle.onTeamSwitch(() => {
  tokens.value = [];
  issuedPlain.value = '';
  void loadTokens();
});

onMounted(() => {
  void loadTokens();
});
</script>

<template>
  <div class="coolify-card">
    <div class="coolify-well" style="display:grid;gap:10px;">
      <strong>API Tokens</strong>
      <form style="display:flex;gap:8px;flex-wrap:wrap;" @submit.prevent="createToken">
        <input v-model="newName" class="coolify-input" placeholder="Token label" aria-label="Token label" required />
        <select v-model="ability" class="coolify-input" aria-label="Token ability">
          <option value="read">read</option>
          <option value="deploy">deploy</option>
          <option value="write">write</option>
          <option value="root">root</option>
        </select>
        <button type="submit" class="coolify-btn coolify-btn-primary">Create Token</button>
      </form>
      <p v-if="status" role="status" style="margin:0;">{{ status }}</p>
      <pre v-if="issuedPlain" style="margin:0;font-family:var(--ui-font-mono);font-size:12px;">{{ issuedPlain }}</pre>
      <ul style="list-style:none;padding:0;margin:0;display:grid;gap:6px;">
        <li
          v-for="tok in tokens"
          :key="tok.id"
          style="display:flex;justify-content:space-between;align-items:center;padding:6px 8px;border:1px solid var(--ui-well-border);border-radius:6px;"
        >
          <span>{{ tok.name }} ({{ (tok.abilities ?? []).join(', ') }})</span>
          <button type="button" class="coolify-btn" @click="revokeToken(tok.id)">Revoke</button>
        </li>
      </ul>
    </div>
  </div>
</template>
