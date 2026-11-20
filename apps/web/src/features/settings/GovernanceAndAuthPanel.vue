<script setup lang="ts">
import { onMounted, ref } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const lifecycle = usePageLifecycle();

const auditEvents = ref<Array<{ id?: number; action: string; resource_type?: string; created_at?: string }>>([]);
const tags = ref<Array<{ id: number; uuid?: string; name: string }>>([]);
const sharedVars = ref<Array<{ id: number; key: string; type: string }>>([]);
const oauthProviders = ref<Array<{ provider: string; enabled: boolean }>>([]);
const subscription = ref<{ subscribed?: boolean; stripe_plan_id?: string | null }>({});
const mcpEnabled = ref(false);
const twoFactorSecret = ref('');
const twoFactorCode = ref('');

const newTagName = ref('');
const newSharedKey = ref('');
const newSharedVal = ref('');
const status = ref('');

async function loadAll() {
  try {
    const [audits, tagList, svList, oauthList, subObj, instSettings] = await Promise.all([
      apiRequest<Array<{ id?: number; action: string; resource_type?: string; created_at?: string }>>('/api/v1/audit-events', { signal: lifecycle.signal }).catch(() => []),
      apiRequest<Array<{ id: number; uuid?: string; name: string }>>('/api/v1/tags', { signal: lifecycle.signal }).catch(() => []),
      apiRequest<Array<{ id: number; key: string; type: string }>>('/api/v1/shared-variables', { signal: lifecycle.signal }).catch(() => []),
      apiRequest<Array<{ provider: string; enabled: boolean }>>('/app/settings/oauth', { signal: lifecycle.signal }).catch(() => []),
      apiRequest<{ subscribed?: boolean; stripe_plan_id?: string | null }>('/api/v1/subscription', { signal: lifecycle.signal }).catch(() => ({} as { subscribed?: boolean; stripe_plan_id?: string | null })),
      apiRequest<{ is_mcp_server_enabled?: boolean }>('/api/v1/settings', { signal: lifecycle.signal }).catch(() => ({} as { is_mcp_server_enabled?: boolean })),
    ]);
    auditEvents.value = audits.slice(0, 12);
    tags.value = tagList;
    sharedVars.value = svList;
    oauthProviders.value = oauthList;
    subscription.value = subObj;
    mcpEnabled.value = Boolean(instSettings.is_mcp_server_enabled);
  } catch {
    // Handled cleanly
  }
}

async function createTag() {
  if (!newTagName.value.trim()) return;
  try {
    await apiRequest('/api/v1/tags', {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({ name: newTagName.value.trim() }),
    });
    newTagName.value = '';
    status.value = 'Tag saved';
    await loadAll();
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function createSharedVar() {
  if (!newSharedKey.value.trim()) return;
  try {
    await apiRequest('/api/v1/shared-variables', {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({ key: newSharedKey.value.trim(), value: newSharedVal.value, type: 'team' }),
    });
    newSharedKey.value = '';
    newSharedVal.value = '';
    status.value = 'Team shared variable saved';
    await loadAll();
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function toggleMCP() {
  try {
    const target = mcpEnabled.value ? '/api/v1/mcp/disable' : '/api/v1/mcp/enable';
    await apiRequest(target, { method: 'POST', signal: lifecycle.signal });
    mcpEnabled.value = !mcpEnabled.value;
    status.value = `MCP server ${mcpEnabled.value ? 'enabled' : 'disabled'}`;
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function enable2FA() {
  try {
    const res = await apiRequest<{ secret: string }>('/app/auth/2fa/enable', { method: 'POST', signal: lifecycle.signal });
    twoFactorSecret.value = res.secret;
    status.value = '2FA secret generated. Enter TOTP code to confirm.';
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function confirm2FA() {
  if (!twoFactorCode.value.trim()) return;
  try {
    await apiRequest('/app/auth/2fa/confirm', {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({ code: twoFactorCode.value.trim() }),
    });
    twoFactorCode.value = '';
    status.value = '2FA confirmed and active';
  } catch (err) {
    status.value = (err as Error).message;
  }
}

lifecycle.onTeamSwitch(() => {
  void loadAll();
});

onMounted(() => {
  void loadAll();
});
</script>

<template>
  <div class="coolify-card" data-testid="governance-auth-panel">
    <div class="coolify-well" style="display:grid;gap:12px;">
      <div style="display:flex;justify-content:space-between;align-items:center;flex-wrap:wrap;gap:8px;">
        <strong>Account 2FA, OAuth, Tags, Shared Variables &amp; Audit Log</strong>
        <div style="display:flex;gap:6px;">
          <button type="button" class="coolify-btn" @click="toggleMCP">MCP: {{ mcpEnabled ? 'Enabled' : 'Disabled' }}</button>
          <button type="button" class="coolify-btn" @click="enable2FA">Setup 2FA TOTP</button>
          <button type="button" class="coolify-btn" @click="loadAll">Refresh</button>
        </div>
      </div>

      <div v-if="twoFactorSecret" style="display:flex;gap:8px;align-items:center;flex-wrap:wrap;">
        <span style="font-family:var(--ui-font-mono);font-size:12px;">Secret: {{ twoFactorSecret }}</span>
        <input v-model="twoFactorCode" class="coolify-input" placeholder="6-digit TOTP code" />
        <button type="button" class="coolify-btn coolify-btn-primary" @click="confirm2FA">Confirm 2FA</button>
      </div>

      <div style="display:grid;grid-template-columns:1fr 1fr;gap:10px;">
        <form style="display:flex;gap:6px;" @submit.prevent="createTag">
          <input v-model="newTagName" class="coolify-input" placeholder="New tag name" required />
          <button type="submit" class="coolify-btn">Add Tag ({{ tags.length }})</button>
        </form>
        <form style="display:flex;gap:6px;" @submit.prevent="createSharedVar">
          <input v-model="newSharedKey" class="coolify-input" placeholder="SHARED_KEY" required />
          <input v-model="newSharedVal" class="coolify-input" placeholder="Value" />
          <button type="submit" class="coolify-btn">Add Shared Var ({{ sharedVars.length }})</button>
        </form>
      </div>

      <div style="font-size:12px;color:var(--ui-fg-dim);">
        OAuth Providers: {{ oauthProviders.length }} configured | Subscription: {{ subscription.subscribed ? 'Active' : 'Self-Hosted / Free' }}
      </div>

      <p v-if="status" role="status" style="margin:0;">{{ status }}</p>

      <div>
        <strong style="font-size:13px;">Recent Team Audit Events ({{ auditEvents.length }})</strong>
        <ul style="list-style:none;padding:0;margin:6px 0 0;display:grid;gap:4px;font-family:var(--ui-font-mono);font-size:12px;">
          <li v-for="(ev, idx) in auditEvents" :key="ev.id ?? idx">
            {{ ev.action }} ({{ ev.resource_type || 'team' }})
          </li>
        </ul>
      </div>
    </div>
  </div>
</template>
