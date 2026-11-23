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
    status: string;
    logs?: string;
    created_at: string;
    updated_at?: string;
}

const rows = ref<DeploymentRow[]>([]);
const activeUuid = ref('');
const detailLogs = ref('');
const detailError = ref('');
const loadingDetail = ref(false);
let pollTimer: ReturnType<typeof setInterval> | null = null;

const activeRow = computed(
    () => rows.value.find((r) => r.deployment_uuid === activeUuid.value) ?? rows.value[0],
);

const isRunning = computed(() =>
    rows.value.some((r) => r.status === 'queued' || r.status === 'in_progress' || r.status === 'running'),
);

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
    <div class="coolify-card">
        <div
            class="coolify-well"
            style="display: grid; gap: 12px"
        >
            <div style="display: flex; justify-content: space-between; align-items: center">
                <strong>Deployment History</strong>
                <button
                    type="button"
                    class="coolify-btn"
                    @click="loadHistory"
                >
                    Refresh
                </button>
            </div>
            <p
                v-if="rows.length === 0"
                style="color: var(--ui-fg-dim); margin: 0"
            >
                No deployments recorded yet.
            </p>
            <template v-else>
                <ul
                    style="
                        list-style: none;
                        padding: 0;
                        margin: 0;
                        display: grid;
                        gap: 6px;
                    "
                >
                    <li
                        v-for="item in rows"
                        :key="item.deployment_uuid"
                        style="
                            display: flex;
                            justify-content: space-between;
                            align-items: center;
                            gap: 8px;
                            padding: 8px 10px;
                            border-radius: 6px;
                            border: 1px solid var(--ui-border);
                            cursor: pointer;
                            font-family: var(--ui-font-mono);
                            font-size: 12px;
                        "
                        :style="{
                            background:
                                activeRow?.deployment_uuid === item.deployment_uuid
                                    ? 'var(--ui-surface-hover)'
                                    : 'transparent',
                        }"
                        @click="selectDeployment(item.deployment_uuid)"
                    >
                        <span>
                            {{ item.deployment_uuid }}: {{ item.status }} ({{ item.created_at }})
                        </span>
                        <div style="display: flex; gap: 6px; align-items: center">
                            <span
                                class="status-badge"
                                :class="{
                                    warning:
                                        item.status !== 'finished' && item.status !== 'running',
                                }"
                            >
                                {{ item.status }}
                            </span>
                            <button
                                v-if="item.status === 'queued' || item.status === 'in_progress'"
                                type="button"
                                class="coolify-btn danger-button"
                                style="padding: 2px 8px; font-size: 11px"
                                @click.stop="cancelDeployment(item.deployment_uuid)"
                            >
                                Cancel
                            </button>
                        </div>
                    </li>
                </ul>

                <div
                    v-if="activeRow"
                    class="deployment-log-panel"
                    style="
                        border-top: 1px solid var(--ui-border);
                        padding-top: 12px;
                        display: grid;
                        gap: 8px;
                    "
                >
                    <div
                        style="
                            display: flex;
                            justify-content: space-between;
                            align-items: center;
                            font-size: 12px;
                        "
                    >
                        <strong>
                            Deployment Logs ({{ activeRow.deployment_uuid }})
                        </strong>
                        <span class="code-text">{{ activeRow.status }}</span>
                    </div>
                    <pre
                        style="
                            margin: 0;
                            padding: 12px;
                            border-radius: 6px;
                            background: #0a0a0c;
                            color: #e4e4e7;
                            font-family: var(--ui-font-mono);
                            font-size: 12px;
                            line-height: 1.5;
                            max-height: 420px;
                            overflow: auto;
                            white-space: pre-wrap;
                            word-break: break-word;
                        "
                        >{{
                            detailLogs ||
                            activeRow.logs ||
                            detailError ||
                            (activeRow.status === 'queued'
                                ? 'Deployment is queued and waiting for worker execution...'
                                : activeRow.status === 'in_progress'
                                  ? 'Deployment is in progress...'
                                  : 'No deployment log output recorded.')
                        }}</pre
                    >
                </div>
            </template>
        </div>
    </div>
</template>
