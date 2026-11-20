<script setup lang="ts">
import { onMounted, ref } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

type ChannelKey = 'email' | 'discord' | 'slack' | 'telegram' | 'pushover' | 'webhook';

const lifecycle = usePageLifecycle();
const selectedChannel = ref<ChannelKey>('webhook');
const channels = ref<Record<ChannelKey, boolean>>({
  email: false,
  discord: false,
  slack: false,
  telegram: false,
  pushover: false,
  webhook: false,
});

const webhookUrl = ref('');
const smtpHost = ref('');
const smtpPort = ref(587);
const smtpFrom = ref('');
const smtpRecipient = ref('');
const telegramToken = ref('');
const telegramChatId = ref('');
const pushoverUser = ref('');
const pushoverToken = ref('');
const status = ref('');

async function loadConfigs() {
  try {
    const list = await apiRequest<Array<{ channel: string; enabled: boolean }>>('/api/v1/notifications', {
      signal: lifecycle.signal,
    });
    for (const item of list) {
      if (item.channel in channels.value) {
        channels.value[item.channel as ChannelKey] = item.enabled;
      }
    }
  } catch {
    // Ignore when unauthenticated
  }
}

async function saveChannel() {
  status.value = '';
  const ch = selectedChannel.value;
  const payload: Record<string, unknown> = { enabled: channels.value[ch] };
  if (ch === 'webhook' || ch === 'discord' || ch === 'slack') {
    payload.webhook_url = webhookUrl.value.trim();
  } else if (ch === 'email') {
    payload.smtp_enabled = channels.value.email;
    payload.smtp_host = smtpHost.value.trim();
    payload.smtp_port = smtpPort.value;
    payload.smtp_from_address = smtpFrom.value.trim();
    payload.smtp_recipients = smtpRecipient.value.trim();
  } else if (ch === 'telegram') {
    payload.telegram_enabled = channels.value.telegram;
    payload.telegram_token = telegramToken.value.trim();
    payload.telegram_chat_id = telegramChatId.value.trim();
  } else if (ch === 'pushover') {
    payload.pushover_enabled = channels.value.pushover;
    payload.pushover_user_key = pushoverUser.value.trim();
    payload.pushover_api_token = pushoverToken.value.trim();
  }
  try {
    await apiRequest(`/api/v1/notifications/${ch}`, {
      method: 'PATCH',
      signal: lifecycle.signal,
      body: JSON.stringify(payload),
    });
    status.value = `Saved ${ch} notification settings`;
    await loadConfigs();
  } catch (err) {
    status.value = (err as Error).message;
  }
}

async function sendTest() {
  status.value = '';
  try {
    await apiRequest('/api/v1/notifications/test', {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({ channel: selectedChannel.value, event: 'test', summary: 'Goolify live test' }),
    });
    status.value = `Test notification delivered via ${selectedChannel.value}`;
  } catch (err) {
    status.value = (err as Error).message;
  }
}

lifecycle.onTeamSwitch(() => {
  for (const k of Object.keys(channels.value) as ChannelKey[]) channels.value[k] = false;
  webhookUrl.value = '';
  status.value = '';
  void loadConfigs();
});

onMounted(() => {
  void loadConfigs();
});
</script>

<template>
  <form class="coolify-card" data-testid="notification-settings" @submit.prevent="saveChannel">
    <div class="coolify-well" style="display:grid;gap:10px;">
      <div style="display:flex;justify-content:space-between;align-items:center;flex-wrap:wrap;gap:8px;">
        <strong>Notifications (6 Laravel-Compatible Channels)</strong>
        <select v-model="selectedChannel" class="coolify-input" aria-label="Notification channel">
          <option value="webhook">Webhook</option>
          <option value="discord">Discord</option>
          <option value="slack">Slack</option>
          <option value="email">Email (SMTP / Resend)</option>
          <option value="telegram">Telegram</option>
          <option value="pushover">Pushover</option>
        </select>
      </div>

      <div style="display:flex;flex-wrap:wrap;gap:12px;">
        <label><input v-model="channels.email" type="checkbox" /> Email</label>
        <label><input v-model="channels.discord" type="checkbox" /> Discord</label>
        <label><input v-model="channels.slack" type="checkbox" /> Slack</label>
        <label><input v-model="channels.telegram" type="checkbox" /> Telegram</label>
        <label><input v-model="channels.pushover" type="checkbox" /> Pushover</label>
        <label><input v-model="channels.webhook" type="checkbox" /> Webhook</label>
      </div>

      <div v-if="selectedChannel === 'webhook' || selectedChannel === 'discord' || selectedChannel === 'slack'" style="display:flex;gap:8px;">
        <input v-model="webhookUrl" class="coolify-input" style="flex:1;" placeholder="Webhook URL (https://...)" aria-label="Webhook URL" />
      </div>

      <div v-else-if="selectedChannel === 'email'" style="display:flex;gap:8px;flex-wrap:wrap;">
        <input v-model="smtpHost" class="coolify-input" placeholder="SMTP Host" />
        <input v-model.number="smtpPort" type="number" class="coolify-input" style="width:90px;" placeholder="Port" />
        <input v-model="smtpFrom" class="coolify-input" placeholder="From address" />
        <input v-model="smtpRecipient" class="coolify-input" placeholder="Recipients" />
      </div>

      <div v-else-if="selectedChannel === 'telegram'" style="display:flex;gap:8px;flex-wrap:wrap;">
        <input v-model="telegramToken" type="password" class="coolify-input" placeholder="Bot Token" />
        <input v-model="telegramChatId" class="coolify-input" placeholder="Chat ID" />
      </div>

      <div v-else-if="selectedChannel === 'pushover'" style="display:flex;gap:8px;flex-wrap:wrap;">
        <input v-model="pushoverUser" class="coolify-input" placeholder="Pushover User Key" />
        <input v-model="pushoverToken" type="password" class="coolify-input" placeholder="Pushover API Token" />
      </div>

      <div style="display:flex;gap:8px;">
        <button type="submit" class="coolify-btn coolify-btn-primary">Save Channel</button>
        <button type="button" class="coolify-btn" @click="sendTest">Send Real Test</button>
      </div>
      <p v-if="status" role="status" style="margin:0;">{{ status }}</p>
    </div>
  </form>
</template>
