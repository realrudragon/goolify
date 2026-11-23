<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue';
import AppIcon from '../../components/AppIcon.vue';
import PageHeading from '../../components/PageHeading.vue';
import SettingsSection from '../../components/SettingsSection.vue';
import MetricsChart from '../../components/MetricsChart.vue';
import DestinationsPage from './DestinationsPage.vue';
import RegistriesPage from './RegistriesPage.vue';
import ProxyConfig from '../proxy/ProxyConfig.vue';
import WebTerminal from '../terminal/WebTerminal.vue';
import LogViewer from '../logs/LogViewer.vue';
import ServerTransferPanel from '../transfer/ServerTransferPanel.vue';
import { apiRequest } from '../../lib/api';
import { navigateTo } from '../../lib/navigation';
import { session } from '../../lib/session';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const props = defineProps<{ serverUuid: string; subpage: string }>();
interface Server {
    id: number;
    uuid: string;
    name: string;
    description?: string;
    ip: string;
    user: string;
    port: number;
    team_id: number;
    is_reachable: boolean;
    is_usable: boolean;
    private_key_uuid?: string;
    transferred_at?: string;
}
interface Field {
    key: string;
    label: string;
    type?: string;
    min?: number;
    max?: number;
}
const lifecycle = usePageLifecycle();
const server = ref<Server>();
const form = ref<Record<string, any>>({});
const initial = ref('');
const keys = ref<{ uuid: string; name?: string }[]>([]);
const resources = ref<{ uuid: string; name: string; type: string; status: string }[]>([]);
const error = ref('');
const message = ref('');
const busy = ref(false);
const confirmDialog = ref<HTMLDialogElement>();
const confirmation = ref('');
const endpoint = computed(() => `/api/v1/servers/${encodeURIComponent(props.serverUuid)}`);
const canEdit = computed(
    () =>
        ['admin', 'owner'].includes(
            session.me?.teams.find((t) => t.id === server.value?.team_id)?.role?.toLowerCase() ??
                '',
        ) && !server.value?.transferred_at,
);
const ready = computed(() => server.value?.is_reachable && server.value?.is_usable);
const dirty = computed(() => JSON.stringify(form.value) !== initial.value);
const menu = [
    ['General', 'general', 'settings'],
    ['Advanced', 'advanced', 'settings'],
    ['Private Key', 'private-key', 'keys'],
    ['Cloud Token', 'cloud-provider-token', 'cloud'],
    ['CA Certificate', 'ca-certificate', 'shield-alert'],
    ['Security', 'security', 'shield-alert'],
    ['Cloudflare Tunnel', 'cloudflare-tunnel', 'cloud'],
    ['Proxy', 'proxy', 'network'],
    ['Sentinel', 'sentinel', 'graph'],
    ['Resources', 'resources', 'projects'],
    ['Terminal', 'terminal', 'browser-terminal'],
    ['Destinations', 'destinations', 'destinations'],
    ['Swarm', 'swarm', 'servers'],
    ['Images', 'images', 'layers'],
    ['Docker Cleanup', 'docker-cleanup', 'layers'],
    ['GitHub Runners', 'github-runners', 'sources'],
    ['Registries', 'registries', 'layers'],
    ['Log Drains', 'log-drains', 'file-content'],
    ['Metrics', 'metrics', 'graph'],
    ['Analytics', 'analytics', 'analytics'],
    ['Transfer', 'transfer', 'network'],
    ['Danger Zone', 'danger', 'shield-alert'],
];
const fields: Record<string, Field[]> = {
    'docker-cleanup': [
        { key: 'docker_cleanup_frequency', label: 'Schedule' },
        {
            key: 'docker_cleanup_threshold',
            label: 'Disk usage threshold (%)',
            type: 'number',
            min: 1,
            max: 99,
        },
        { key: 'force_docker_cleanup', label: 'Force Docker cleanup', type: 'checkbox' },
        { key: 'delete_unused_volumes', label: 'Delete unused volumes', type: 'checkbox' },
        { key: 'delete_unused_networks', label: 'Delete unused networks', type: 'checkbox' },
        {
            key: 'disable_application_image_retention',
            label: 'Disable application image retention',
            type: 'checkbox',
        },
    ],
    sentinel: [
        { key: 'is_sentinel_enabled', label: 'Enable Sentinel', type: 'checkbox' },
        { key: 'is_metrics_enabled', label: 'Enable metrics', type: 'checkbox' },
        { key: 'sentinel_custom_url', label: 'Custom Sentinel URL' },
        {
            key: 'sentinel_push_interval_seconds',
            label: 'Push interval (seconds)',
            type: 'number',
            min: 1,
        },
        {
            key: 'sentinel_metrics_refresh_rate_seconds',
            label: 'Metrics refresh rate (seconds)',
            type: 'number',
            min: 1,
        },
    ],
    'cloudflare-tunnel': [
        { key: 'is_cloudflare_tunnel', label: 'Enable Cloudflare Tunnel', type: 'checkbox' },
        { key: 'cloudflare_tunnel_id', label: 'Tunnel ID' },
        { key: 'cloudflare_tunnel_account_id', label: 'Account ID' },
    ],
    'log-drains': [
        { key: 'is_logdrain_axiom_enabled', label: 'Enable Axiom', type: 'checkbox' },
        { key: 'logdrain_axiom_dataset_name', label: 'Dataset name' },
        { key: 'logdrain_axiom_api_key', label: 'API key', type: 'password' },
        { key: 'is_logdrain_newrelic_enabled', label: 'Enable New Relic', type: 'checkbox' },
        { key: 'logdrain_newrelic_base_uri', label: 'Base URI' },
        { key: 'logdrain_newrelic_license_key', label: 'License key', type: 'password' },
        { key: 'is_logdrain_custom_enabled', label: 'Enable custom log drain', type: 'checkbox' },
        { key: 'logdrain_custom_config', label: 'Custom configuration', type: 'textarea' },
        { key: 'logdrain_custom_config_parser', label: 'Custom parser', type: 'textarea' },
    ],
    advanced: [
        { key: 'server_timezone', label: 'Server Timezone' },
        { key: 'wildcard_domain', label: 'Wildcard Domain' },
        { key: 'concurrent_builds', label: 'Concurrent Builds', type: 'number', min: 1, max: 32 },
        {
            key: 'server_disk_usage_notification_threshold',
            label: 'Disk Usage Alert Threshold (%)',
            type: 'number',
            min: 1,
            max: 100,
        },
        { key: 'is_build_server', label: 'Use as Build Server', type: 'checkbox' },
        { key: 'is_terminal_enabled', label: 'Enable Web Terminal', type: 'checkbox' },
        { key: 'generate_exact_labels', label: 'Generate Exact Traefik Labels', type: 'checkbox' },
    ],
    swarm: [
        { key: 'is_swarm_manager', label: 'Docker Swarm Manager', type: 'checkbox' },
        { key: 'is_swarm_worker', label: 'Docker Swarm Worker', type: 'checkbox' },
    ],
    images: [
        { key: 'is_image_scan_enabled', label: 'Enable Automatic Image Vulnerability Scanning', type: 'checkbox' },
    ],
    'github-runners': [
        { key: 'enabled', label: 'Enable GitHub Actions Runner', type: 'checkbox' },
        { key: 'repository_url', label: 'Repository / Org URL' },
        { key: 'runner_labels', label: 'Runner Labels (comma-separated)' },
    ],
};
const subpageApiPath: Record<string, string> = {
    swarm: 'advanced',
    images: 'image-scanning',
};
const metricsData = ref<{
    has_metrics: boolean;
    latest?: { cpu_percent: number; memory_percent: number; disk_percent: number; recorded_at: string };
    history: { cpu_percent: number; memory_percent: number; disk_percent: number; recorded_at: string }[];
}>({ has_metrics: false, history: [] });
const containersData = ref<Record<string, any>[]>([]);
const subTitle = computed(() => menu.find((m) => m[1] === props.subpage)?.[0] ?? props.subpage);
async function load() {
    error.value = '';
    try {
        server.value = await apiRequest<Server>(endpoint.value, { signal: lifecycle.signal });
        if (props.subpage === 'general')
            form.value = {
                name: server.value.name,
                description: server.value.description ?? '',
                ip: server.value.ip,
                user: server.value.user,
                port: server.value.port,
                wildcard_domain: (server.value as any).wildcard_domain ?? '',
            };
        else if (props.subpage === 'private-key') {
            keys.value = await apiRequest('/api/v1/security/keys', { signal: lifecycle.signal });
            form.value = { private_key_uuid: server.value.private_key_uuid ?? '' };
        } else if (fields[props.subpage]) {
            const apiSub = subpageApiPath[props.subpage] ?? props.subpage;
            form.value = await apiRequest(`${endpoint.value}/${apiSub}`, {
                signal: lifecycle.signal,
            });
        } else if (props.subpage === 'resources')
            resources.value = await apiRequest(`${endpoint.value}/resources`, {
                signal: lifecycle.signal,
            });
        else if (props.subpage === 'metrics') {
            metricsData.value = await apiRequest(`${endpoint.value}/metrics`, {
                signal: lifecycle.signal,
            });
        } else if (props.subpage === 'analytics') {
            const [m, c] = await Promise.all([
                apiRequest<typeof metricsData.value>(`${endpoint.value}/metrics`, {
                    signal: lifecycle.signal,
                }),
                apiRequest<{ containers?: Record<string, any>[] }>(
                    `/canvas/api/servers/${encodeURIComponent(props.serverUuid)}/canvas/containers`,
                    { signal: lifecycle.signal },
                ),
            ]);
            metricsData.value = m;
            containersData.value = c.containers ?? [];
        }
        initial.value = JSON.stringify(form.value);
    } catch (err) {
        error.value = (err as Error).message;
    }
}
async function save() {
    if (busy.value || !canEdit.value) return;
    busy.value = true;
    error.value = '';
    message.value = '';
    try {
        const body = fields[props.subpage]
            ? Object.fromEntries(fields[props.subpage].map((f) => [f.key, form.value[f.key]]))
            : form.value;
        const apiSub = subpageApiPath[props.subpage] ?? props.subpage;
        const method = props.subpage === 'github-runners' ? 'POST' : 'PATCH';
        await apiRequest(endpoint.value + (fields[props.subpage] ? `/${apiSub}` : ''), {
            method,
            body: JSON.stringify(body),
        });
        await load();
        message.value = 'Saved.';
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        busy.value = false;
    }
}
async function action(path: string) {
    if (!canEdit.value || busy.value) return;
    busy.value = true;
    error.value = '';
    try {
        const result = await apiRequest<{ message?: string }>(`${endpoint.value}/${path}`, {
            method: 'POST',
            body: '{}',
        });
        message.value = result.message ?? 'Operation queued.';
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        busy.value = false;
    }
}
async function remove() {
    if (confirmation.value !== server.value?.name || server.value?.id === 0 || !canEdit.value)
        return;
    busy.value = true;
    try {
        await apiRequest(endpoint.value, { method: 'DELETE' });
        navigateTo('/servers');
    } catch (err) {
        error.value = (err as Error).message;
        busy.value = false;
    }
}
watch(
    () => [props.serverUuid, props.subpage],
    () => {
        message.value = '';
        error.value = '';
        void load();
    },
);
onMounted(load);
</script>

<template>
    <section
        class="original-page"
        data-testid="server-page"
    >
        <PageHeading
            :title="server?.name ?? 'Server'"
            :description="server ? `${server.user}@${server.ip}` : undefined"
            ><span
                v-if="server"
                class="status-badge"
                :class="{ warning: !ready }"
                >{{
                    server.transferred_at
                        ? 'Transferred away'
                        : ready
                          ? 'Ready'
                          : 'Validation required'
                }}</span
            ></PageHeading
        >
        <div class="settings-workspace">
            <nav
                class="settings-sidebar"
                aria-label="Server configuration sections"
            >
                <template
                    v-for="[label, key, icon] in menu"
                    :key="key"
                    ><a
                        :href="`/server/${serverUuid}${key === 'general' ? '' : `/${key}`}`"
                        :aria-current="
                            subpage === key || subpage.startsWith(`${key}/`) ? 'page' : undefined
                        "
                        ><AppIcon
                            :name="icon"
                            :size="14"
                        />{{ label }}</a
                    ><template v-if="key === 'proxy' && subpage.startsWith('proxy')"
                        ><a
                            v-for="[text, suffix] in [
                                ['Configuration', ''],
                                ['Dynamic Configurations', '/dynamic'],
                                ['TLS Certificates', '/certificates'],
                                ['Logs', '/logs'],
                            ]"
                            :key="suffix"
                            class="settings-sub-link"
                            :href="`/server/${serverUuid}/proxy${suffix}`"
                            :aria-current="subpage === `proxy${suffix}` ? 'page' : undefined"
                            >{{ text }}</a
                        ></template
                    ></template
                >
            </nav>
            <div class="settings-content">
                <p
                    v-if="error"
                    role="alert"
                    class="page-error"
                >
                    {{ error }}
                </p>
                <p
                    v-if="message"
                    role="status"
                >
                    {{ message }}
                </p>
                <template v-if="server">
                    <form
                        v-if="subpage === 'general'"
                        @submit.prevent="save"
                    >
                        <div
                            v-if="dirty && canEdit"
                            class="unsaved-bar"
                        >
                            <span>You have unsaved changes</span>
                            <div class="page-actions">
                                <button
                                    type="button"
                                    class="coolify-btn"
                                    @click="load"
                                >
                                    Discard</button
                                ><button
                                    type="submit"
                                    class="coolify-btn coolify-btn-primary"
                                    :disabled="busy"
                                >
                                    Save
                                </button>
                            </div>
                        </div>
                        <div class="settings-content">
                            <SettingsSection
                                v-if="server.id === 0"
                                title="Server overview"
                                helper="Operating system and hardware details for the server running this Goolify instance."
                                ><template #actions
                                    ><button
                                        v-if="canEdit"
                                        type="button"
                                        class="coolify-btn"
                                        :disabled="busy"
                                        @click="action('validate')"
                                    >
                                        <AppIcon
                                            name="refresh3"
                                            :size="14"
                                        />{{
                                            ready ? 'Revalidate connection' : 'Validate connection'
                                        }}
                                    </button></template
                                >
                                <div class="resource-name">
                                    <span class="resource-glyph"><AppIcon name="servers" /></span>
                                    <div>
                                        <strong>Localhost</strong>
                                        <p class="page-muted">
                                            {{
                                                ready
                                                    ? 'The server is reachable, validated, and ready to host resources.'
                                                    : 'Validate the local Docker connection before using this server.'
                                            }}
                                        </p>
                                    </div>
                                </div></SettingsSection
                            >
                            <SettingsSection
                                title="Connection"
                                helper="Configure how Goolify identifies, reaches, and validates this server."
                                ><template #actions
                                    ><button
                                        v-if="server.id !== 0 && canEdit"
                                        type="button"
                                        class="coolify-btn"
                                        :disabled="busy"
                                        @click="action('validate')"
                                    >
                                        {{
                                            ready ? 'Revalidate connection' : 'Validate connection'
                                        }}</button
                                    ><button
                                        v-if="canEdit"
                                        type="submit"
                                        class="coolify-btn coolify-btn-primary"
                                        :disabled="busy"
                                    >
                                        Save
                                    </button></template
                                >
                                <div class="form-grid">
                                    <label class="form-field"
                                        >Name<input
                                            v-model="form.name"
                                            class="coolify-input"
                                            required
                                            :readonly="!canEdit" /></label
                                    ><label class="form-field"
                                        >Description<input
                                            v-model="form.description"
                                            class="coolify-input"
                                            :readonly="!canEdit" /></label
                                    ><label class="form-field"
                                        >IP address or domain<input
                                            v-model="form.ip"
                                            class="coolify-input"
                                            required
                                            :readonly="!canEdit" /></label
                                    ><label class="form-field"
                                        >Wildcard Domain<input
                                            v-model="form.wildcard_domain"
                                            class="coolify-input"
                                            placeholder="https://example.com"
                                            :readonly="!canEdit" /></label
                                    ><label class="form-field"
                                        >SSH user<input
                                            v-model="form.user"
                                            class="coolify-input"
                                            required
                                            :readonly="!canEdit" /></label
                                    ><label class="form-field"
                                        >SSH port<input
                                            v-model.number="form.port"
                                            type="number"
                                            min="1"
                                            max="65535"
                                            class="coolify-input"
                                            required
                                            :readonly="!canEdit"
                                    /></label></div
                            ></SettingsSection>
                        </div>
                    </form>
                    <form
                        v-else-if="fields[subpage] || subpage === 'private-key'"
                        @submit.prevent="save"
                    >
                        <SettingsSection :title="subTitle"
                            ><template #actions
                                ><button
                                    v-if="canEdit"
                                    type="submit"
                                    class="coolify-btn coolify-btn-primary"
                                    :disabled="busy || !dirty"
                                >
                                    Save</button
                                ><button
                                    v-if="subpage === 'docker-cleanup' && canEdit"
                                    type="button"
                                    class="coolify-btn"
                                    :disabled="busy"
                                    @click="action('docker-cleanup/run')"
                                >
                                    Run cleanup
                                </button></template
                            >
                            <div class="form-grid">
                                <label
                                    v-if="subpage === 'private-key'"
                                    class="form-field full-span"
                                    >Private Key<select
                                        v-model="form.private_key_uuid"
                                        class="coolify-input"
                                        :disabled="!canEdit"
                                        required
                                    >
                                        <option
                                            value=""
                                            disabled
                                        >
                                            Select a private key
                                        </option>
                                        <option
                                            v-for="key in keys"
                                            :key="key.uuid"
                                            :value="key.uuid"
                                        >
                                            {{ key.name ?? key.uuid }}
                                        </option>
                                    </select></label
                                ><template
                                    v-for="field in fields[subpage] ?? []"
                                    :key="field.key"
                                    ><label
                                        v-if="field.type === 'checkbox'"
                                        class="check-field full-span"
                                        ><input
                                            v-model="form[field.key]"
                                            type="checkbox"
                                            :disabled="!canEdit"
                                        />{{ field.label }}</label
                                    ><label
                                        v-else
                                        class="form-field"
                                        :class="{ 'full-span': field.type === 'textarea' }"
                                        >{{ field.label
                                        }}<textarea
                                            v-if="field.type === 'textarea'"
                                            v-model="form[field.key]"
                                            class="coolify-input"
                                            rows="6"
                                            :readonly="!canEdit" /><input
                                            v-else
                                            v-model="form[field.key]"
                                            :type="field.type ?? 'text'"
                                            :min="field.min"
                                            :max="field.max"
                                            class="coolify-input"
                                            :readonly="!canEdit" /></label
                                ></template></div
                        ></SettingsSection>
                    </form>
                    <DestinationsPage
                        v-else-if="subpage === 'destinations'"
                        :server-uuid="serverUuid"
                    />
                    <RegistriesPage
                        v-else-if="subpage === 'registries'"
                        :server-uuid="serverUuid"
                    />
                    <WebTerminal
                        v-else-if="subpage === 'terminal'"
                        :server-uuid="serverUuid"
                    />
                    <LogViewer
                        v-else-if="subpage.endsWith('/logs')"
                        :resource-uuid="serverUuid"
                    />
                    <ProxyConfig
                        v-else-if="subpage === 'proxy'"
                        :server-uuid="serverUuid"
                    />
                    <ServerTransferPanel
                        v-else-if="subpage === 'transfer'"
                        :server-uuid="serverUuid"
                    />
                    <SettingsSection
                        v-else-if="subpage === 'resources'"
                        title="Resources"
                        ><div class="original-table-scroll">
                            <table class="original-table">
                                <thead>
                                    <tr>
                                        <th>Name</th>
                                        <th>Type</th>
                                        <th>Status</th>
                                    </tr>
                                </thead>
                                <tbody>
                                    <tr
                                        v-for="resource in resources"
                                        :key="resource.uuid"
                                    >
                                        <td>{{ resource.name }}</td>
                                        <td>{{ resource.type }}</td>
                                        <td>{{ resource.status }}</td>
                                    </tr>
                                </tbody>
                            </table>
                        </div>
                        <p
                            v-if="!resources.length"
                            class="page-muted"
                        >
                            No resources found
                        </p></SettingsSection
                    >
                    <SettingsSection
                        v-else-if="subpage === 'metrics'"
                        title="Metrics"
                        ><template #actions
                            ><button
                                type="button"
                                class="coolify-btn"
                                :disabled="busy"
                                @click="load"
                            >
                                Refresh
                            </button></template
                        >
                        <div
                            v-if="metricsData.has_metrics && metricsData.latest"
                            class="form-grid"
                        >
                            <div class="form-field">
                                <strong>CPU Usage</strong>
                                <p>{{ Math.round(metricsData.latest.cpu_percent) }}%</p>
                            </div>
                            <div class="form-field">
                                <strong>Memory Usage</strong>
                                <p>{{ Math.round(metricsData.latest.memory_percent) }}%</p>
                            </div>
                            <div class="form-field">
                                <strong>Disk Usage</strong>
                                <p>{{ Math.round(metricsData.latest.disk_percent) }}%</p>
                            </div>
                        </div>
                        <MetricsChart
                            v-if="metricsData.history.length"
                            :history="metricsData.history"
                        />
                        <div
                            v-if="metricsData.history.length"
                            class="original-table-scroll"
                        >
                            <table class="original-table">
                                <thead>
                                    <tr>
                                        <th>Recorded At</th>
                                        <th>CPU</th>
                                        <th>Memory</th>
                                        <th>Disk</th>
                                    </tr>
                                </thead>
                                <tbody>
                                    <tr
                                        v-for="(row, idx) in metricsData.history.slice(0, 20)"
                                        :key="idx"
                                    >
                                        <td class="code-text">{{ row.recorded_at }}</td>
                                        <td>{{ Math.round(row.cpu_percent) }}%</td>
                                        <td>{{ Math.round(row.memory_percent) }}%</td>
                                        <td>{{ Math.round(row.disk_percent) }}%</td>
                                    </tr>
                                </tbody>
                            </table>
                        </div>
                        <p
                            v-else
                            class="page-muted"
                        >
                            No metrics samples recorded yet for this server.
                        </p></SettingsSection
                    >
                    <SettingsSection
                        v-else-if="subpage === 'analytics'"
                        title="Analytics"
                        ><div
                            v-if="metricsData.has_metrics && metricsData.latest"
                            class="form-grid"
                        >
                            <div class="form-field">
                                <strong>CPU</strong>
                                <p>{{ Math.round(metricsData.latest.cpu_percent) }}%</p>
                            </div>
                            <div class="form-field">
                                <strong>Memory</strong>
                                <p>{{ Math.round(metricsData.latest.memory_percent) }}%</p>
                            </div>
                            <div class="form-field">
                                <strong>Disk</strong>
                                <p>{{ Math.round(metricsData.latest.disk_percent) }}%</p>
                            </div>
                        </div>
                        <div
                            v-if="containersData.length"
                            class="original-table-scroll"
                        >
                            <table class="original-table">
                                <thead>
                                    <tr>
                                        <th>Container</th>
                                        <th>Status</th>
                                    </tr>
                                </thead>
                                <tbody>
                                    <tr
                                        v-for="(c, idx) in containersData"
                                        :key="idx"
                                    >
                                        <td class="code-text">{{ c.name ?? c.id }}</td>
                                        <td>{{ c.status ?? 'running' }}</td>
                                    </tr>
                                </tbody>
                            </table>
                        </div>
                        <p
                            v-else
                            class="page-muted"
                        >
                            No container telemetry reported yet.
                        </p></SettingsSection
                    >
                    <SettingsSection
                        v-else-if="subpage === 'danger'"
                        title="Danger zone"
                        ><div class="danger-row">
                            <div>
                                <strong>Delete server</strong>
                                <p>Permanently remove this server from Goolify.</p>
                            </div>
                            <button
                                class="coolify-btn danger-button"
                                type="button"
                                :disabled="server.id === 0 || !canEdit"
                                @click="confirmDialog?.showModal()"
                            >
                                Delete server
                            </button>
                        </div></SettingsSection
                    >
                    <SettingsSection
                        v-else
                        :title="subTitle"
                        ><p
                            class="page-error"
                            role="alert"
                        >
                            This page has not been restored yet.
                        </p></SettingsSection
                    >
                </template>
            </div>
        </div>
        <dialog
            ref="confirmDialog"
            class="workspace-dialog"
        >
            <form
                class="dialog-form"
                @submit.prevent="remove"
            >
                <h2>Confirm Server Deletion?</h2>
                <label class="form-field"
                    >Server Name<input
                        v-model="confirmation"
                        class="coolify-input"
                        required
                /></label>
                <p
                    v-if="error"
                    role="alert"
                >
                    {{ error }}
                </p>
                <div class="page-actions">
                    <button
                        type="button"
                        class="coolify-btn"
                        @click="confirmDialog?.close()"
                    >
                        Cancel</button
                    ><button
                        type="submit"
                        class="coolify-btn danger-button"
                        :disabled="busy || confirmation !== server?.name"
                    >
                        Permanently Delete
                    </button>
                </div>
            </form>
        </dialog>
    </section>
</template>
