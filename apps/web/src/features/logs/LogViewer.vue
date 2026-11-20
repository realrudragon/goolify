<script setup lang="ts">
import { onMounted, ref, watch } from 'vue';
import { subscribeEventStream, type StreamLogLine } from '../../lib/realtime';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const props = defineProps<{ resourceUuid: string }>();
const lifecycle = usePageLifecycle();
const lines = ref<StreamLogLine[]>([]);
const gapDetected = ref(false);
let activeStop: (() => void) | null = null;

function reconnect() {
  activeStop?.();
  activeStop = null;
  lines.value = [];
  gapDetected.value = false;
  if (!props.resourceUuid) return;
  activeStop = subscribeEventStream(
    `/realtime/logs?resource=${encodeURIComponent(props.resourceUuid)}`,
    (line) => {
      // Bound browser memory to last 500 lines; rendered strictly via Vue text interpolation
      lines.value = [...lines.value.slice(-499), line];
    },
    () => {
      gapDetected.value = true;
    },
    () => {},
  );
}

watch(
  () => props.resourceUuid,
  () => {
    reconnect();
  },
);

lifecycle.onTeamSwitch(() => {
  activeStop?.();
  activeStop = null;
  lines.value = [];
});

lifecycle.registerCleanup(() => {
  activeStop?.();
  activeStop = null;
});

onMounted(() => {
  reconnect();
});
</script>

<template>
  <div class="coolify-card">
    <div class="coolify-well" style="font-family:var(--ui-font-mono);font-size:12px;">
      <div style="margin-bottom:6px;font-family:var(--ui-font-sans);"><strong>Live Log Stream ({{ resourceUuid }})</strong></div>
      <p v-if="gapDetected" role="status">Older log entries were pruned (cursor gap).</p>
      <p v-if="lines.length === 0" style="color:var(--ui-fg-dim);margin:0;">Waiting for log lines...</p>
      <pre v-for="l in lines" :key="l.cursor" data-testid="log-line" style="margin:0;white-space:pre-wrap;">{{ l.text }}</pre>
    </div>
  </div>
</template>
