<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

export interface CanvasNode {
  id: string;
  nodeType: string;
  nodeUuid: string;
  label: string;
  kind: 'server' | 'application' | 'database' | 'service';
  x: number;
  y: number;
  status: 'running' | 'stopped' | 'degraded';
}

export interface CanvasEdge {
  from: string;
  to: string;
}

interface SavedPosition {
  node_type: string;
  node_uuid: string;
  x: number;
  y: number;
}

const props = withDefaults(
  defineProps<{
    nodes?: CanvasNode[];
    edges?: CanvasEdge[];
  }>(),
  {
    nodes: undefined,
    edges: undefined,
  },
);

const lifecycle = usePageLifecycle();
const localNodes = ref<CanvasNode[]>(props.nodes ? props.nodes.map((n) => ({ ...n })) : []);
const localEdges = ref<CanvasEdge[]>(props.edges ? props.edges.map((e) => ({ ...e })) : []);
const selectedId = ref<string | null>(localNodes.value[0]?.id ?? null);
const saveStatus = ref('');

const nodeMap = computed(() => {
  const m = new Map<string, CanvasNode>();
  for (const n of localNodes.value) m.set(n.id, n);
  return m;
});

async function loadTopology() {
  if (props.nodes) return;
  try {
    const [srvs, apps, svcs, dbs, canvasState] = await Promise.all([
      apiRequest<Array<{ uuid: string; name: string; is_reachable?: boolean }>>('/api/v1/servers', { signal: lifecycle.signal }).catch(() => []),
      apiRequest<Array<{ uuid: string; name: string; server_uuid?: string; status?: string }>>('/api/v1/applications', { signal: lifecycle.signal }).catch(() => []),
      apiRequest<Array<{ uuid: string; name: string; server_uuid?: string; status?: string }>>('/api/v1/services', { signal: lifecycle.signal }).catch(() => []),
      apiRequest<Array<{ uuid: string; name: string; server_uuid?: string; status?: string }>>('/api/v1/databases', { signal: lifecycle.signal }).catch(() => []),
      apiRequest<{ positions?: SavedPosition[] | Record<string, { x: number; y: number }> }>('/canvas/api/dashboard/canvas', { signal: lifecycle.signal }).catch(() => ({ positions: {} })),
    ]);

    const savedPos = new Map<string, { x: number; y: number }>();
    const rawPositions = canvasState.positions;
    if (Array.isArray(rawPositions)) {
      for (const p of rawPositions) {
        savedPos.set(`${p.node_type}:${p.node_uuid}`, { x: p.x, y: p.y });
      }
    } else if (rawPositions && typeof rawPositions === 'object') {
      for (const [k, coords] of Object.entries(rawPositions)) {
        if (coords && typeof coords.x === 'number' && typeof coords.y === 'number') {
          savedPos.set(k, { x: coords.x, y: coords.y });
        }
      }
    }

    const nodes: CanvasNode[] = [];
    const edges: CanvasEdge[] = [];
    srvs.forEach((s, idx) => {
      const pos = savedPos.get(`server:${s.uuid}`) ?? { x: 24, y: 24 + idx * 64 };
      nodes.push({
        id: `srv-${s.uuid}`,
        nodeType: 'server',
        nodeUuid: s.uuid,
        label: s.name,
        kind: 'server',
        x: pos.x,
        y: pos.y,
        status: s.is_reachable === false ? 'degraded' : 'running',
      });
    });

    apps.forEach((a, idx) => {
      const pos = savedPos.get(`application:${a.uuid}`) ?? { x: 220, y: 24 + idx * 58 };
      const id = `app-${a.uuid}`;
      nodes.push({
        id,
        nodeType: 'application',
        nodeUuid: a.uuid,
        label: a.name,
        kind: 'application',
        x: pos.x,
        y: pos.y,
        status: a.status === 'running' ? 'running' : 'stopped',
      });
      if (a.server_uuid) edges.push({ from: `srv-${a.server_uuid}`, to: id });
    });

    svcs.forEach((sv, idx) => {
      const pos = savedPos.get(`service:${sv.uuid}`) ?? { x: 420, y: 24 + idx * 58 };
      const id = `svc-${sv.uuid}`;
      nodes.push({
        id,
        nodeType: 'service',
        nodeUuid: sv.uuid,
        label: sv.name,
        kind: 'service',
        x: pos.x,
        y: pos.y,
        status: sv.status === 'running' ? 'running' : 'stopped',
      });
      if (sv.server_uuid) edges.push({ from: `srv-${sv.server_uuid}`, to: id });
    });

    dbs.forEach((d, idx) => {
      const pos = savedPos.get(`database:${d.uuid}`) ?? { x: 220, y: 140 + idx * 58 };
      const dbNodeId = `db-${d.uuid}`;
      nodes.push({
        id: dbNodeId,
        nodeType: 'database',
        nodeUuid: d.uuid,
        label: d.name,
        kind: 'database',
        x: pos.x,
        y: pos.y,
        status: d.status === 'running' ? 'running' : 'stopped',
      });
      if (d.server_uuid) edges.push({ from: `srv-${d.server_uuid}`, to: dbNodeId });
    });

    localNodes.value = nodes;
    localEdges.value = edges;
    selectedId.value = nodes[0]?.id ?? null;
  } catch {
    localNodes.value = [];
    localEdges.value = [];
    selectedId.value = null;
  }
}

async function persistPositions() {
  if (localNodes.value.length === 0) return;
  try {
    await apiRequest('/canvas/api/dashboard/canvas/positions', {
      method: 'PATCH',
      signal: lifecycle.signal,
      body: JSON.stringify({
        positions: localNodes.value.map((n) => ({
          node_type: n.nodeType,
          node_uuid: n.nodeUuid,
          x: n.x,
          y: n.y,
        })),
      }),
    });
    saveStatus.value = 'Canvas positions saved to PostgreSQL';
  } catch (err) {
    saveStatus.value = (err as Error).message;
  }
}

function moveSelected(dx: number, dy: number) {
  if (!selectedId.value) return;
  localNodes.value = localNodes.value.map((n) =>
    n.id === selectedId.value ? { ...n, x: Math.max(0, n.x + dx), y: Math.max(0, n.y + dy) } : n,
  );
  void persistPositions();
}

lifecycle.onTeamSwitch(() => {
  localNodes.value = [];
  localEdges.value = [];
  selectedId.value = null;
  saveStatus.value = '';
  void loadTopology();
});

onMounted(() => {
  void loadTopology();
});

defineExpose({ refresh: loadTopology });
</script>

<template>
  <section
    class="coolify-card"
    tabindex="0"
    aria-label="Resource topology canvas"
    data-testid="canvas-graph"
    @keydown.arrow-right.prevent="moveSelected(16, 0)"
    @keydown.arrow-left.prevent="moveSelected(-16, 0)"
    @keydown.arrow-down.prevent="moveSelected(0, 16)"
    @keydown.arrow-up.prevent="moveSelected(0, -16)"
  >
    <div class="coolify-well">
      <div style="display:flex;justify-content:space-between;align-items:center;gap:8px;flex-wrap:wrap;margin-bottom:8px;">
        <strong>Infrastructure Canvas ({{ localNodes.length }} live nodes)</strong>
        <div style="display:flex;gap:6px;align-items:center;">
          <span v-if="selectedId" style="font-size:12px;color:var(--ui-fg-dim);">Selected: {{ selectedId }}</span>
          <button type="button" class="coolify-btn" @click="moveSelected(16, 0)">Nudge Right</button>
          <button type="button" class="coolify-btn" @click="persistPositions">Save Layout</button>
          <button type="button" class="coolify-btn" @click="loadTopology">Refresh</button>
        </div>
      </div>
      <p v-if="saveStatus" role="status" style="margin:0 0 8px;font-size:12px;color:var(--ui-fg-dim);">{{ saveStatus }}</p>
      <svg width="100%" height="260" viewBox="0 0 640 260" role="img" aria-label="Topology graph">
        <line
          v-for="edge in localEdges"
          :key="`${edge.from}-${edge.to}`"
          :x1="(nodeMap.get(edge.from)?.x ?? 0) + 150"
          :y1="(nodeMap.get(edge.from)?.y ?? 0) + 22"
          :x2="nodeMap.get(edge.to)?.x ?? 0"
          :y2="(nodeMap.get(edge.to)?.y ?? 0) + 22"
          stroke="var(--ui-fg-faint)"
          stroke-width="1.5"
        />
        <g
          v-for="node in localNodes"
          :key="node.id"
          style="cursor:pointer;"
          @click="selectedId = node.id"
        >
          <rect
            :x="node.x"
            :y="node.y"
            width="160"
            height="44"
            rx="7"
            fill="var(--ui-panel)"
            :stroke="selectedId === node.id ? 'var(--ui-accent)' : 'var(--ui-card-border)'"
            :stroke-width="selectedId === node.id ? 2 : 1"
          />
          <text :x="node.x + 10" :y="node.y + 18" fill="var(--ui-fg)" font-size="12" font-weight="600">
            {{ node.label }}
          </text>
          <text :x="node.x + 10" :y="node.y + 34" fill="var(--ui-fg-dim)" font-size="10">
            {{ node.kind }}: {{ node.status }}
          </text>
        </g>
      </svg>
    </div>
  </section>
</template>
