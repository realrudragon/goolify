<script setup lang="ts">
import { computed, ref } from 'vue';

export interface MetricPoint {
    cpu_percent: number;
    memory_percent: number;
    disk_percent: number;
    recorded_at: string;
}

const props = defineProps<{
    history: MetricPoint[];
}>();

const range = ref<'15m' | '1h' | '24h' | 'all'>('all');
const hoveredIdx = ref<number | null>(null);

const ordered = computed(() => {
    const chrono = [...props.history].reverse();
    if (range.value === 'all' || chrono.length === 0) return chrono;
    const now = Date.now();
    const windowMs =
        range.value === '15m'
            ? 15 * 60 * 1000
            : range.value === '1h'
              ? 60 * 60 * 1000
              : 24 * 60 * 60 * 1000;
    const filtered = chrono.filter((p) => {
        const ts = new Date(p.recorded_at).getTime();
        return Number.isFinite(ts) && now - ts <= windowMs;
    });
    return filtered.length > 0 ? filtered : chrono;
});

const W = 640;
const H = 180;
const PAD_X = 36;
const PAD_Y = 14;
const innerW = W - PAD_X * 2;
const innerH = H - PAD_Y * 2;

function pointX(idx: number, total: number): number {
    if (total <= 1) return PAD_X + innerW / 2;
    return PAD_X + (idx / (total - 1)) * innerW;
}

function pointY(pct: number): number {
    const clamped = Math.max(0, Math.min(100, Number(pct) || 0));
    return PAD_Y + innerH - (clamped / 100) * innerH;
}

function buildPolyline(field: 'cpu_percent' | 'memory_percent' | 'disk_percent'): string {
    const pts = ordered.value;
    if (!pts.length) return '';
    return pts.map((p, i) => `${pointX(i, pts.length).toFixed(1)},${pointY(p[field]).toFixed(1)}`).join(' ');
}

const cpuLine = computed(() => buildPolyline('cpu_percent'));
const memLine = computed(() => buildPolyline('memory_percent'));
const diskLine = computed(() => buildPolyline('disk_percent'));
const activePoint = computed(() => {
    const pts = ordered.value;
    if (!pts.length) return null;
    const idx = hoveredIdx.value !== null && hoveredIdx.value < pts.length ? hoveredIdx.value : pts.length - 1;
    return { ...pts[idx], idx };
});
</script>

<template>
    <div class="metrics-chart-card">
        <div class="metrics-chart-toolbar">
            <div class="metrics-legend">
                <span class="legend-dot legend-cpu">CPU</span>
                <span class="legend-dot legend-mem">Memory</span>
                <span class="legend-dot legend-disk">Disk</span>
            </div>
            <div
                class="metrics-range-group"
                role="group"
                aria-label="Metrics time range"
            >
                <button
                    v-for="opt in (['15m', '1h', '24h', 'all'] as const)"
                    :key="opt"
                    type="button"
                    class="coolify-btn"
                    :class="{ 'coolify-btn-primary': range === opt }"
                    @click="range = opt"
                >
                    {{ opt === 'all' ? 'All' : opt }}
                </button>
            </div>
        </div>

        <div
            v-if="activePoint"
            class="metrics-readout"
        >
            <span>CPU: <strong>{{ Math.round(activePoint.cpu_percent) }}%</strong></span>
            <span>Memory: <strong>{{ Math.round(activePoint.memory_percent) }}%</strong></span>
            <span>Disk: <strong>{{ Math.round(activePoint.disk_percent) }}%</strong></span>
            <span class="page-muted">{{ new Date(activePoint.recorded_at).toLocaleTimeString() }}</span>
        </div>

        <svg
            v-if="ordered.length"
            :viewBox="`0 0 ${W} ${H}`"
            class="metrics-svg"
            role="img"
            aria-label="Server CPU, Memory, and Disk usage history"
            @mouseleave="hoveredIdx = null"
        >
            <g
                v-for="tick in [0, 25, 50, 75, 100]"
                :key="tick"
            >
                <line
                    :x1="PAD_X"
                    :x2="W - PAD_X"
                    :y1="pointY(tick)"
                    :y2="pointY(tick)"
                    class="metrics-grid-line"
                />
                <text
                    :x="PAD_X - 6"
                    :y="pointY(tick) + 3"
                    text-anchor="end"
                    class="metrics-axis-label"
                >
                    {{ tick }}%
                </text>
            </g>

            <polyline
                v-if="diskLine"
                :points="diskLine"
                fill="none"
                class="metrics-line metrics-line-disk"
            />
            <polyline
                v-if="memLine"
                :points="memLine"
                fill="none"
                class="metrics-line metrics-line-mem"
            />
            <polyline
                v-if="cpuLine"
                :points="cpuLine"
                fill="none"
                class="metrics-line metrics-line-cpu"
            />

            <g
                v-for="(pt, idx) in ordered"
                :key="idx"
            >
                <circle
                    :cx="pointX(idx, ordered.length)"
                    :cy="pointY(pt.cpu_percent)"
                    :r="hoveredIdx === idx ? 4 : 2.2"
                    class="metrics-dot-cpu"
                />
                <circle
                    :cx="pointX(idx, ordered.length)"
                    :cy="pointY(pt.memory_percent)"
                    :r="hoveredIdx === idx ? 4 : 2.2"
                    class="metrics-dot-mem"
                />
                <rect
                    :x="pointX(idx, ordered.length) - Math.max(8, innerW / Math.max(ordered.length, 1) / 2)"
                    :y="PAD_Y"
                    :width="Math.max(16, innerW / Math.max(ordered.length, 1))"
                    :height="innerH"
                    fill="transparent"
                    @mouseenter="hoveredIdx = idx"
                />
            </g>
        </svg>
    </div>
</template>
