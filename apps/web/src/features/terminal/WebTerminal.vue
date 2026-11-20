<script setup lang="ts">
import { onMounted, ref } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const props = defineProps<{ serverUuid: string; container?: string }>();
const lifecycle = usePageLifecycle();
const output = ref<string[]>([]);
const commandInput = ref('');
const connected = ref(false);
const error = ref('');
let socket: WebSocket | null = null;

async function connectTerminal() {
  error.value = '';
  socket?.close();
  socket = null;
  try {
    const { ticket } = await apiRequest<{ ticket: string }>('/app/terminal/ticket', {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({ server_uuid: props.serverUuid, container: props.container ?? '' }),
    });

    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    socket = new WebSocket(`${proto}//${window.location.host}/terminal/ws?ticket=${encodeURIComponent(ticket)}`);
    socket.addEventListener('open', () => {
      connected.value = true;
    });
    socket.addEventListener('message', (ev) => {
      output.value = [...output.value.slice(-400), String(ev.data)];
    });
    socket.addEventListener('close', () => {
      connected.value = false;
    });
  } catch (err) {
    if ((err as Error).name !== 'AbortError') {
      error.value = (err as Error).message || 'Terminal connection failed';
    }
  }
}

function sendLine() {
  if (socket && connected.value && commandInput.value) {
    socket.send(JSON.stringify({ type: 'stdin', data: commandInput.value + '\n' }));
    commandInput.value = '';
  }
}

lifecycle.registerCleanup(() => {
  socket?.close();
  socket = null;
});

lifecycle.onTeamSwitch(() => {
  socket?.close();
  socket = null;
  connected.value = false;
  output.value = [];
});

onMounted(() => {
  void connectTerminal();
});
</script>

<template>
  <div class="coolify-card">
    <div class="coolify-well" style="font-family:var(--ui-font-mono);">
      <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:8px;font-family:var(--ui-font-sans);">
        <span data-testid="terminal-status">Terminal Status: {{ connected ? 'Connected' : 'Disconnected' }}</span>
        <button type="button" class="coolify-btn" @click="connectTerminal">Reconnect</button>
      </div>
      <p v-if="error" role="alert" style="color:var(--ui-error);margin:0 0 8px;">{{ error }}</p>
      <pre data-testid="terminal-output" style="min-height:120px;margin:0 0 8px;white-space:pre-wrap;">{{ output.join('') }}</pre>
      <form style="display:flex;gap:6px;" @submit.prevent="sendLine">
        <input v-model="commandInput" class="coolify-input" style="flex:1;" placeholder="Enter shell command..." aria-label="Terminal command" />
        <button type="submit" class="coolify-btn coolify-btn-primary">Send</button>
      </form>
    </div>
  </div>
</template>
