<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const props = defineProps<{
    applicationUuid: string;
    selectedDeploymentUuid?: string;
}>();
const lifecycle = usePageLifecycle();

interface DeploymentRow {
    deployment_uuid: string;
    operation_id?: string;
    server_uuid?: string;
    status: string;
    logs?: string;
    is_webhook?: boolean;
    is_api?: boolean;
    created_at: string;
    updated_at?: string;
}

const rows = ref<DeploymentRow[]>([]);
const activeUuid = ref('');
const detailLogs = ref('');
const detailError = ref('');
const loadingDetail = ref(false);
const searchDeployments = ref('');
const statusFilter = ref('');
const sortAsc = ref(false);
const page = ref(1);
const pageSize = 6;
const logSearch = ref('');
const showTimestamps = ref(true);
const fullscreen = ref(false);
const exportMenuOpen = ref(false);
let pollTimer: ReturnType<typeof setInterval> | null = null;

const filteredRows = computed(() => {
    const q = searchDeployments.value.trim().toLowerCase();
    const list = rows.value.filter((r) => {
        if (statusFilter.value && r.status !== statusFilter.value) return false;
        if (!q) return true;
        return (
            r.deployment_uuid.toLowerCase().includes(q) ||
            r.status.toLowerCase().includes(q) ||
            (r.server_uuid ?? '').toLowerCase().includes(q)
        );
    });
    return sortAsc.value ? [...list].reverse() : list;
});

const totalPages = computed(() => Math.max(1, Math.ceil(filteredRows.value.length / pageSize)));
const pagedRows = computed(() => {
    const start = (page.value - 1) * pageSize;
    return filteredRows.value.slice(start, start + pageSize);
});

const activeRow = computed(
    () => rows.value.find((r) => r.deployment_uuid === activeUuid.value) ?? rows.value[0],
);

const isRunning = computed(() =>
    rows.value.some(
        (r) => r.status === 'queued' || r.status === 'in_progress' || r.status === 'running',
    ),
);

const rawLogText = computed(() => {
    if (detailLogs.value) return detailLogs.value;
    if (activeRow.value?.logs) return activeRow.value.logs;
    if (detailError.value) return detailError.value;
    if (!activeRow.value) return '';
    if (activeRow.value.status === 'queued') {
        return 'Deployment is queued and waiting for worker execution...';
    }
    if (activeRow.value.status === 'in_progress') {
        return 'Deployment is in progress...';
    }
    return 'No deployment log output recorded.';
});

const parsedLogLines = computed(() => {
    const q = logSearch.value.trim().toLowerCase();
    const tsFallback = activeRow.value?.updated_at || activeRow.value?.created_at || '';
    const formattedFallback = formatLogTimestamp(tsFallback);
    return rawLogText.value
        .split(/\r?\n/)
        .filter((line, idx, arr) => line !== '' || idx < arr.length - 1)
        .map((line, index) => {
            const lower = line.toLowerCase();
            const isError =
                lower.includes('[error]') ||
                lower.includes('error:') ||
                lower.includes('fatal:') ||
                lower.includes('failed') ||
                lower.includes('not found') ||
                lower.includes('permission denied') ||
                lower.includes('cannot connect');
            const isWarning = lower.includes('warning') || lower.includes('warn');
            const matches = !q || lower.includes(q);
            return {
                index,
                timestamp: formattedFallback,
                text: line,
                isError,
                isWarning,
                matches,
            };
        });
});

const visibleLogLines = computed(() =>
    logSearch.value.trim()
        ? parsedLogLines.value.filter((l) => l.matches)
        : parsedLogLines.value,
);

function formatLogTimestamp(iso?: string): string {
    if (!iso) return '';
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return iso.slice(0, 19).replace('T', ' ');
    const months = [
        'Jan',
        'Feb',
        'Mar',
        'Apr',
        'May',
        'Jun',
        'Jul',
        'Aug',
        'Sep',
        'Oct',
        'Nov',
        'Dec',
    ];
    const pad = (n: number) => String(n).padStart(2, '0');
    return `${d.getFullYear()}-${months[d.getMonth()]}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

function formatRelative(iso?: string): string {
    if (!iso) return '';
    const diffSec = Math.max(0, Math.floor((Date.now() - new Date(iso).getTime()) / 1000));
    if (diffSec < 60) return `${diffSec}s`;
    const mins = Math.floor(diffSec / 60);
    if (mins < 60) return `${mins}m`;
    const hrs = Math.floor(mins / 60);
    if (hrs < 24) return `${hrs}h`;
    return `${Math.floor(hrs / 24)}d`;
}

function formatDuration(row: DeploymentRow): string {
    if (!row.created_at || !row.updated_at) return '00m 00s';
    const sec = Math.max(
        0,
        Math.floor(
            (new Date(row.updated_at).getTime() - new Date(row.created_at).getTime()) / 1000,
        ),
    );
    const m = String(Math.floor(sec / 60)).padStart(2, '0');
    const s = String(sec % 60).padStart(2, '0');
    return `${m}m ${s}s`;
}

function statusDotColor(st?: string): string {
    if (st === 'finished' || st === 'running') return '#10b981';
    if (st === 'failed') return '#ef4444';
    if (st === 'queued' || st === 'in_progress') return '#f59e0b';
    return '#71717a';
}

function statusLabel(st?: string): string {
    if (!st) return 'Unknown';
    if (st === 'in_progress') return 'In progress';
    return st.charAt(0).toUpperCase() + st.slice(1);
}

async function loadDetail(depUuid: string) {
    if (!depUuid) return;
    loadingDetail.value = true;
    try {
        const detail = await apiRequest<{
            logs?: string;
            last_error?: string;
            payload?: { logs?: string };
        }>(`/api/v1/deployments/${encodeURIComponent(depUuid)}`, { signal: lifecycle.signal });
        const rowLogs = rows.value.find((r) => r.deployment_uuid === depUuid)?.logs ?? '';
        detailLogs.value = detail.logs || detail.payload?.logs || rowLogs || '';
        detailError.value = detail.last_error || '';
    } catch {
        const row = rows.value.find((r) => r.deployment_uuid === depUuid);
        detailLogs.value = row?.logs ?? '';
    } finally {
        loadingDetail.value = false;
    }
}

async function loadHistory() {
    if (!props.applicationUuid) {
        rows.value = [];
        return;
    }
    try {
        const list = await apiRequest<DeploymentRow[]>(
            `/api/v1/deployments/applications/${encodeURIComponent(props.applicationUuid)}`,
            { signal: lifecycle.signal },
        );
        rows.value = list;
        const target =
            props.selectedDeploymentUuid ||
            activeUuid.value ||
            (list.length > 0 ? list[0].deployment_uuid : '');
        if (target) {
            activeUuid.value = target;
            await loadDetail(target);
        }
    } catch {
        rows.value = [];
    }
}

async function selectDeployment(depUuid: string) {
    activeUuid.value = depUuid;
    await loadDetail(depUuid);
}

async function cancelDeployment(depUuid: string) {
    try {
        await apiRequest(`/api/v1/deployments/${encodeURIComponent(depUuid)}/cancel`, {
            method: 'POST',
            body: '{}',
        });
        await loadHistory();
    } catch {
        // ignore cancel error
    }
}

async function copyLogs() {
    exportMenuOpen.value = false;
    try {
        await navigator.clipboard.writeText(rawLogText.value);
    } catch {
        // ignore clipboard error
    }
}

function downloadLogs() {
    exportMenuOpen.value = false;
    const blob = new Blob([rawLogText.value], { type: 'text/plain;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `deployment-${activeRow.value?.deployment_uuid || 'logs'}.txt`;
    a.click();
    URL.revokeObjectURL(url);
}

watch(
    () => [props.applicationUuid, props.selectedDeploymentUuid],
    () => {
        if (props.selectedDeploymentUuid) {
            activeUuid.value = props.selectedDeploymentUuid;
        }
        void loadHistory();
    },
);

lifecycle.onTeamSwitch(() => {
    rows.value = [];
    activeUuid.value = '';
    detailLogs.value = '';
});

onMounted(() => {
    if (props.selectedDeploymentUuid) {
        activeUuid.value = props.selectedDeploymentUuid;
    }
    void loadHistory();
    pollTimer = setInterval(() => {
        if (isRunning.value) {
            void loadHistory();
        }
    }, 2000);
});

onBeforeUnmount(() => {
    if (pollTimer) {
        clearInterval(pollTimer);
        pollTimer = null;
    }
});

defineExpose({ refresh: loadHistory });
</script>

<template>
    <div
        class="deployment-split-layout"
        :class="{ 'is-fullscreen': fullscreen }"
    >
        <!-- Left Pane: Deployment History (Coolify v4.7.6) -->
        <section
            v-if="!fullscreen"
            class="coolify-card deployment-history-pane"
        >
            <header class="coolify-card-header">
                <h2 class="coolify-underlined-title">Deployment History</h2>
                <button
                    type="button"
                    class="coolify-btn"
                    @click="loadHistory"
                >
                    Refresh
                </button>
            </header>

            <div class="deployment-toolbar">
                <input
                    v-model="searchDeployments"
                    type="search"
                    class="coolify-input"
                    placeholder="Search deployments"
                    aria-label="Search deployments"
                    @input="page = 1"
                />
                <select
                    v-model="statusFilter"
                    class="coolify-input"
                    aria-label="Filter deployments"
                    style="width: auto; min-width: 90px"
                    @change="page = 1"
                >
                    <option value="">Filter</option>
                    <option value="finished">Finished</option>
                    <option value="failed">Failed</option>
                    <option value="in_progress">In progress</option>
                    <option value="queued">Queued</option>
                    <option value="cancelled">Cancelled</option>
                </select>
                <button
                    type="button"
                    class="coolify-btn"
                    @click="sortAsc = !sortAsc"
                >
                    {{ sortAsc ? '↑ Sort' : '↓ Sort' }}
                </button>
            </div>

            <div class="deployment-list-header">
                <span>Commit</span>
                <span>Status</span>
            </div>

            <p
                v-if="filteredRows.length === 0"
                class="page-muted"
                style="padding: 18px; margin: 0"
            >
                No deployments recorded yet.
            </p>
            <ul
                v-else
                class="deployment-list"
            >
                <li
                    v-for="item in pagedRows"
                    :key="item.deployment_uuid"
                    class="deployment-row"
                    :class="{ active: activeRow?.deployment_uuid === item.deployment_uuid }"
                    @click="selectDeployment(item.deployment_uuid)"
                >
                    <div class="deployment-row-main">
                        <div class="deployment-row-title">
                            <strong class="code-text">{{ item.deployment_uuid.slice(0, 7) }}</strong>
                            <span class="deployment-row-desc">
                                {{ item.deployment_uuid }} ({{ item.status }})
                            </span>
                        </div>
                        <div class="deployment-row-meta">
                            <span>{{
                                item.is_webhook ? 'Webhook' : item.is_api ? 'API' : 'Manual'
                            }}</span>
                            <span>•</span>
                            <span>{{ formatDuration(item) }}</span>
                            <span>•</span>
                            <span>{{ item.server_uuid || 'localhost' }}</span>
                        </div>
                    </div>
                    <div class="deployment-row-status">
                        <span>{{ formatRelative(item.created_at) }}</span>
                        <span
                            class="status-dot"
                            :style="{ background: statusDotColor(item.status) }"
                            :title="item.status"
                        ></span>
                    </div>
                </li>
            </ul>

            <footer
                v-if="filteredRows.length > 0"
                class="deployment-pagination"
            >
                <span class="page-muted">
                    {{ (page - 1) * pageSize + 1 }}-{{
                        Math.min(page * pageSize, filteredRows.length)
                    }}
                    of {{ filteredRows.length }}
                </span>
                <div style="display: flex; gap: 6px">
                    <button
                        type="button"
                        class="coolify-btn"
                        :disabled="page <= 1"
                        @click="page = Math.max(1, page - 1)"
                    >
                        ←
                    </button>
                    <button
                        type="button"
                        class="coolify-btn"
                        :disabled="page >= totalPages"
                        @click="page = Math.min(totalPages, page + 1)"
                    >
                        →
                    </button>
                </div>
            </footer>
        </section>

        <!-- Right Pane: Deployment Log Terminal (Coolify v4.7.6) -->
        <section class="coolify-card deployment-log-panel">
            <header class="deployment-log-toolbar">
                <div style="display: flex; align-items: center; gap: 10px">
                    <button
                        type="button"
                        class="chrome-button"
                        :title="fullscreen ? 'Exit fullscreen' : 'Fullscreen'"
                        @click="fullscreen = !fullscreen"
                    >
                        {{ fullscreen ? '⤡' : '⤢' }}
                    </button>
                    <span
                        v-if="activeRow"
                        style="display: inline-flex; align-items: center; gap: 6px; font-size: 13px; font-weight: 600"
                    >
                        <span
                            class="status-dot"
                            :style="{ background: statusDotColor(activeRow.status) }"
                        ></span>
                        {{ statusLabel(activeRow.status) }}
                    </span>
                </div>

                <div style="display: flex; align-items: center; gap: 8px; flex-wrap: wrap">
                    <input
                        v-model="logSearch"
                        type="search"
                        class="coolify-input"
                        placeholder="Find in logs (Ctrl+F)"
                        aria-label="Find in logs"
                        style="width: 190px; height: 32px; font-size: 12px"
                    />
                    <button
                        type="button"
                        class="coolify-btn"
                        @click="showTimestamps = !showTimestamps"
                    >
                        {{ showTimestamps ? 'Hide timestamps' : 'Show timestamps' }}
                    </button>
                    <div style="position: relative">
                        <button
                            type="button"
                            class="coolify-btn"
                            @click="exportMenuOpen = !exportMenuOpen"
                        >
                            Export ⌄
                        </button>
                        <div
                            v-if="exportMenuOpen"
                            class="coolify-dropdown-menu"
                        >
                            <button
                                type="button"
                                @click="copyLogs"
                            >
                                Copy logs
                            </button>
                            <button
                                type="button"
                                @click="downloadLogs"
                            >
                                Download .txt
                            </button>
                        </div>
                    </div>
                    <button
                        v-if="
                            activeRow &&
                            (activeRow.status === 'queued' || activeRow.status === 'in_progress')
                        "
                        type="button"
                        class="coolify-btn danger-button"
                        @click="cancelDeployment(activeRow.deployment_uuid)"
                    >
                        Cancel
                    </button>
                </div>
            </header>

            <div
                id="logsContainer"
                class="deployment-log-viewport"
            >
                <p
                    v-if="!activeRow"
                    class="page-muted"
                    style="margin: 0"
                >
                    Select a deployment to inspect its build logs.
                </p>
                <div
                    v-else
                    class="deployment-log-lines"
                >
                    <div
                        v-for="line in visibleLogLines"
                        :key="line.index"
                        class="deployment-log-line"
                    >
                        <span
                            v-if="showTimestamps"
                            class="deployment-log-ts"
                            >{{ line.timestamp }}</span
                        >
                        <span
                            class="deployment-log-text"
                            :class="{
                                'is-error': line.isError,
                                'is-warning': !line.isError && line.isWarning,
                            }"
                            >{{ line.text }}</span
                        >
                    </div>
                </div>
            </div>
        </section>
    </div>
</template>
