<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import PageHeading from '../../components/PageHeading.vue';
import SettingsSection from '../../components/SettingsSection.vue';
import AppIcon from '../../components/AppIcon.vue';
import MetricsChart from '../../components/MetricsChart.vue';
import SecretInput from '../../components/SecretInput.vue';
import DeploymentHistory from '../deployments/DeploymentHistory.vue';
import ScheduleList from '../schedules/ScheduleList.vue';
import BackupManager from '../backups/BackupManager.vue';
import LogViewer from '../logs/LogViewer.vue';
import WebTerminal from '../terminal/WebTerminal.vue';
import { apiRequest } from '../../lib/api';
import { pushToast } from '../../lib/toast';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const props = defineProps<{
    kind: 'application' | 'service' | 'database';
    uuid: string;
    subpage: string;
    projectUuid?: string;
    environmentUuid?: string;
}>();
const lifecycle = usePageLifecycle();
const resource = ref<Record<string, any>>();
const permissions = ref({ update: false, deploy: false, delete: false });
const form = ref<Record<string, any>>({});
const initial = ref('');
const error = ref('');
const status = ref('');
const busy = ref(false);
const rows = ref<Record<string, any>[]>([]);
const envKey = ref('');
const envValue = ref('');
const envPreview = ref(false);
const envDialog = ref<HTMLDialogElement>();
const storageDialog = ref<HTMLDialogElement>();
const composeDialog = ref<HTMLDialogElement>();
const deleteDialog = ref<HTMLDialogElement>();
const confirmation = ref('');
const storages = ref<{ persistent_storages: Record<string, any>[]; file_storages: Record<string, any>[] }>({
    persistent_storages: [],
    file_storages: [],
});
const storageForm = ref({
    type: 'persistent' as 'persistent' | 'file',
    name: '',
    host_path: '',
    fs_path: '',
    mount_path: '',
    content: '',
});
const keys = ref<{ uuid: string; name?: string }[]>([]);
const deployments = ref<Record<string, any>[]>([]);
const attachedDestinations = ref<Record<string, any>[]>([]);
const allDestinations = ref<Record<string, any>[]>([]);
const selectedDestinationUuid = ref('');
const serverMetrics = ref<{
    has_metrics: boolean;
    latest?: { cpu_percent: number; memory_percent: number; disk_percent: number; recorded_at: string };
    history: { cpu_percent: number; memory_percent: number; disk_percent: number; recorded_at: string }[];
}>({ has_metrics: false, history: [] });
const advancedSettings = ref<Record<string, boolean>>({});
const advancedFlags: [string, string][] = [
    ['is_auto_deploy_enabled', 'Auto Deploy'],
    ['is_force_https_enabled', 'Force HTTPS'],
    ['is_preview_deployments_enabled', 'Preview Deployments'],
    ['is_git_submodules_enabled', 'Git Submodules'],
    ['is_git_lfs_enabled', 'Git LFS'],
    ['is_git_shallow_clone_enabled', 'Git Shallow Clone'],
    ['is_spa', 'Is SPA (Single Page Application)'],
    ['is_debug_enabled', 'Debug Logging'],
    ['disable_build_cache', 'Disable Build Cache'],
    ['redact_all_env_values_in_logs', 'Redact All Env Values in Logs'],
];
const endpoint = computed(
    () =>
        `/api/v1/${props.kind === 'application' ? 'applications' : props.kind === 'service' ? 'services' : 'databases'}/${encodeURIComponent(props.uuid)}`,
);
const base = computed(
    () =>
        `/project/${props.projectUuid ?? resource.value?.project_uuid}/environment/${props.environmentUuid ?? resource.value?.environment_uuid}/${props.kind}/${props.uuid}`,
);
const section = computed(() => props.subpage.split('/')[0]);
const dirty = computed(
    () => JSON.stringify({ form: form.value, advanced: advancedSettings.value }) !== initial.value,
);
const canEdit = computed(() => permissions.value.update);
const canDeploy = computed(() => permissions.value.deploy);
const menu = computed(() => [
    ['General', 'general', 'settings'],
    ...(props.kind === 'application'
        ? [
              ['Advanced', 'advanced', 'layers'],
              ['Environment Variables', 'environment-variables', 'variables'],
              ['Persistent Storage', 'storage', 'storages'],
              ['Source', 'source', 'sources'],
              ['Servers', 'servers', 'servers'],
              ['Scheduled Tasks', 'scheduled-tasks', 'time-back'],
              ['Previews', 'previews', 'projects'],
              ['Healthcheck', 'healthcheck', 'graph'],
              ['Rollback', 'rollback', 'time-back'],
              ['Resource Limits', 'resource-limits', 'sliders'],
          ]
        : props.kind === 'service'
          ? [
                ['Environment Variables', 'environment-variables', 'variables'],
                ['Scheduled Tasks', 'scheduled-tasks', 'time-back'],
                ['Persistent Storage', 'storage', 'storages'],
            ]
          : [['Backup', 'backups', 'storages']]),
    ['Logs', 'logs', 'file-content'],
    ['Terminal', 'terminal', 'browser-terminal'],
    ['Metrics', 'metrics', 'graph'],
    ['Tags', 'tags', 'tags'],
    ['Danger Zone', 'danger', 'shield-alert'],
]);
async function load() {
    error.value = '';
    try {
        resource.value = await apiRequest(endpoint.value, { signal: lifecycle.signal });
        if (
            (props.projectUuid && props.projectUuid !== resource.value?.project_uuid) ||
            (props.environmentUuid && props.environmentUuid !== resource.value?.environment_uuid)
        ) {
            resource.value = undefined;
            throw new Error('Resource not found in this environment.');
        }
        permissions.value = await apiRequest(
            `/api/v1/resource-permissions?type=${props.kind}&uuid=${encodeURIComponent(props.uuid)}`,
            { signal: lifecycle.signal },
        );
        const item = resource.value!;
        const sMap: Record<string, string> = item.settings ?? {};
        advancedSettings.value = Object.fromEntries(
            advancedFlags.map(([k]) => [k, sMap[k] === 'true']),
        );
        form.value =
            props.kind === 'application'
                ? {
                      name: item.name,
                      description: item.description ?? '',
                      fqdn: item.fqdn ?? '',
                      git_repository: item.git_repository ?? '',
                      git_branch: item.git_branch ?? '',
                      build_pack: item.build_pack ?? 'dockerfile',
                      ports_exposes: item.ports_exposes ?? '',
                      base_directory: item.base_directory ?? '/',
                      publish_directory: item.publish_directory ?? '',
                      dockerfile_location: item.dockerfile_location ?? '/Dockerfile',
                      dockerfile: item.dockerfile ?? '',
                      docker_compose_location: item.docker_compose_location ?? '/docker-compose.yaml',
                      docker_compose_raw: item.docker_compose_raw ?? '',
                      docker_registry_image_name: item.docker_registry_image_name ?? '',
                      docker_registry_image_tag: item.docker_registry_image_tag ?? '',
                      private_key_uuid: item.private_key_uuid ?? '',
                      custom_labels: item.custom_labels ?? '',
                      custom_docker_run_options: item.custom_docker_run_options ?? '',
                      health_check_enabled: Boolean(item.health_check_enabled),
                      health_check_method: item.health_check_method ?? 'GET',
                      health_check_scheme: item.health_check_scheme ?? 'http',
                      health_check_host: item.health_check_host ?? 'localhost',
                      health_check_port: item.health_check_port ?? '',
                      health_check_path: item.health_check_path ?? '/',
                      health_check_return_code: item.health_check_return_code ?? 200,
                      health_check_response_text: item.health_check_response_text ?? '',
                      health_check_interval: item.health_check_interval ?? 5,
                      health_check_timeout: item.health_check_timeout ?? 5,
                      health_check_retries: item.health_check_retries ?? 10,
                      health_check_start_period: item.health_check_start_period ?? 5,
                      limits_memory: item.limits_memory ?? '0',
                      limits_memory_swap: item.limits_memory_swap ?? '0',
                      limits_memory_swappiness: item.limits_memory_swappiness ?? 60,
                      limits_memory_reservation: item.limits_memory_reservation ?? '0',
                      limits_cpus: item.limits_cpus ?? '0',
                      limits_cpuset: item.limits_cpuset ?? '',
                      limits_cpu_shares: item.limits_cpu_shares ?? 1024,
                  }
                : props.kind === 'service'
                  ? {
                        name: item.name,
                        description: item.description ?? '',
                        docker_compose_raw: item.docker_compose_raw ?? '',
                    }
                  : {
                        name: item.name,
                        description: item.description ?? '',
                        image: item.image ?? '',
                        is_public: Boolean(item.is_public),
                        public_port: item.public_port ?? item.custom_port ?? 0,
                        custom_port: item.custom_port ?? item.public_port ?? 0,
                    };
        initial.value = JSON.stringify({ form: form.value, advanced: advancedSettings.value });
        if (section.value === 'environment-variables')
            rows.value = await apiRequest(`${endpoint.value}/envs`, { signal: lifecycle.signal });
        if (section.value === 'storage')
            storages.value = await apiRequest(`${endpoint.value}/storages`, {
                signal: lifecycle.signal,
            });
        if (section.value === 'source' && props.kind === 'application')
            keys.value = await apiRequest('/api/v1/security/keys', { signal: lifecycle.signal });
        if (section.value === 'rollback' && props.kind === 'application')
            deployments.value = await apiRequest(`${endpoint.value}/deployments`, {
                signal: lifecycle.signal,
            });
        if (section.value === 'servers' && props.kind === 'application') {
            const [attached, available] = await Promise.all([
                apiRequest<Record<string, any>[]>(`${endpoint.value}/destinations`, {
                    signal: lifecycle.signal,
                }),
                apiRequest<Record<string, any>[]>('/api/v1/destinations', {
                    signal: lifecycle.signal,
                }),
            ]);
            attachedDestinations.value = attached;
            allDestinations.value = available;
        }
        if (section.value === 'metrics' && item.server_uuid) {
            serverMetrics.value = await apiRequest(
                `/api/v1/servers/${encodeURIComponent(item.server_uuid)}/metrics`,
                { signal: lifecycle.signal },
            );
        }
        if (section.value === 'tags')
            rows.value = await apiRequest(`${endpoint.value}/tags`, { signal: lifecycle.signal });
        if (section.value === 'previews')
            rows.value = await apiRequest(`${endpoint.value}/previews`, {
                signal: lifecycle.signal,
            });
        if (props.kind === 'service' && section.value === 'general') {
            const [apps, dbs] = await Promise.all([
                apiRequest<Record<string, any>[]>(`${endpoint.value}/applications`, {
                    signal: lifecycle.signal,
                }),
                apiRequest<Record<string, any>[]>(`${endpoint.value}/databases`, {
                    signal: lifecycle.signal,
                }),
            ]);
            rows.value = [
                ...apps.map((r) => ({ ...r, kind: 'application' })),
                ...dbs.map((r) => ({ ...r, kind: 'database' })),
            ];
        }
    } catch (err) {
        error.value = (err as Error).message;
    }
}
async function save() {
    if (!canEdit.value || busy.value) return;
    busy.value = true;
    error.value = '';
    try {
        const body: Record<string, any> = { ...form.value };
        if (props.kind === 'application') {
            body.settings = Object.fromEntries(
                Object.entries(advancedSettings.value).map(([k, v]) => [k, v ? 'true' : 'false']),
            );
        }
        await apiRequest(endpoint.value, { method: 'PATCH', body: JSON.stringify(body) });
        composeDialog.value?.close();
        await load();
        status.value = 'Saved.';
        pushToast('Settings saved.', 'success');
    } catch (err) {
        error.value = (err as Error).message;
        pushToast(error.value, 'error');
    } finally {
        busy.value = false;
    }
}
async function attachDestination() {
    if (!canEdit.value || busy.value || !selectedDestinationUuid.value) return;
    busy.value = true;
    error.value = '';
    try {
        await apiRequest(`${endpoint.value}/destinations`, {
            method: 'POST',
            body: JSON.stringify({ destination_uuid: selectedDestinationUuid.value }),
        });
        selectedDestinationUuid.value = '';
        await load();
        pushToast('Secondary destination attached.', 'success');
    } catch (err) {
        error.value = (err as Error).message;
        pushToast(error.value, 'error');
    } finally {
        busy.value = false;
    }
}
async function detachDestination(destUuid: string) {
    if (!canEdit.value || busy.value) return;
    busy.value = true;
    error.value = '';
    try {
        await apiRequest(`${endpoint.value}/destinations/${encodeURIComponent(destUuid)}`, {
            method: 'DELETE',
        });
        await load();
        pushToast('Secondary destination detached.', 'success');
    } catch (err) {
        error.value = (err as Error).message;
        pushToast(error.value, 'error');
    } finally {
        busy.value = false;
    }
}
async function addStorage() {
    if (!canEdit.value || busy.value) return;
    busy.value = true;
    error.value = '';
    try {
        await apiRequest(`${endpoint.value}/storages`, {
            method: 'POST',
            body: JSON.stringify(storageForm.value),
        });
        storageForm.value = {
            type: 'persistent',
            name: '',
            host_path: '',
            fs_path: '',
            mount_path: '',
            content: '',
        };
        storageDialog.value?.close();
        await load();
        status.value = 'Storage added.';
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        busy.value = false;
    }
}
async function removeStorage(storageUuid: string) {
    try {
        await apiRequest(`${endpoint.value}/storages/${encodeURIComponent(storageUuid)}`, {
            method: 'DELETE',
        });
        await load();
    } catch (err) {
        error.value = (err as Error).message;
    }
}
async function rotateDatabasePassword() {
    if (!canEdit.value || busy.value || props.kind !== 'database') return;
    busy.value = true;
    error.value = '';
    try {
        await apiRequest(`${endpoint.value}/rotate-password`, { method: 'POST', body: '{}' });
        await load();
        status.value = 'Database password rotated.';
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        busy.value = false;
    }
}
async function rollbackTo(deploymentUuid: string) {
    if (!canDeploy.value || busy.value) return;
    busy.value = true;
    error.value = '';
    try {
        const res = await apiRequest<{ deployment_uuid?: string }>(
            `${endpoint.value}/rollback`,
            { method: 'POST', body: JSON.stringify({ commit: deploymentUuid }) },
        );
        if (res.deployment_uuid) {
            window.location.assign(`${base.value}/deployment/${res.deployment_uuid}`);
        } else {
            status.value = 'Rollback queued.';
        }
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        busy.value = false;
    }
}
async function action(name: string) {
    if (!canDeploy.value || busy.value) return;
    busy.value = true;
    error.value = '';
    try {
        if (name === 'deploy' && props.kind === 'application') {
            const result = await apiRequest<{
                deployment_uuid?: string;
                deployments?: { deployment_uuid: string }[];
            }>(`/api/v1/deploy?uuid=${encodeURIComponent(props.uuid)}`, { method: 'POST' });
            const deployment = result.deployment_uuid ?? result.deployments?.[0]?.deployment_uuid;
            if (deployment) window.location.assign(`${base.value}/deployment/${deployment}`);
            else status.value = 'Deployment queued.';
        } else {
            await apiRequest(`${endpoint.value}/${name}`, { method: 'POST' });
            status.value = `${name === 'start' ? 'Start' : name === 'stop' ? 'Stop' : 'Restart'} queued.`;
        }
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        busy.value = false;
    }
}
async function addEnv() {
    busy.value = true;
    error.value = '';
    try {
        await apiRequest(`${endpoint.value}/envs`, {
            method: 'POST',
            body: JSON.stringify({
                key: envKey.value,
                value: envValue.value,
                is_preview: envPreview.value,
            }),
        });
        envKey.value = '';
        envValue.value = '';
        envDialog.value?.close();
        await load();
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        busy.value = false;
    }
}
async function removeEnv(uuid: string) {
    try {
        await apiRequest(`${endpoint.value}/envs/${encodeURIComponent(uuid)}`, {
            method: 'DELETE',
        });
        await load();
    } catch (err) {
        error.value = (err as Error).message;
    }
}
async function remove() {
    if (!permissions.value.delete || confirmation.value !== resource.value?.name) return;
    busy.value = true;
    try {
        await apiRequest(endpoint.value, { method: 'DELETE' });
        window.location.assign(
            `/project/${props.projectUuid}/environment/${props.environmentUuid}`,
        );
    } catch (err) {
        error.value = (err as Error).message;
        busy.value = false;
    }
}
onMounted(load);
</script>

<template>
    <section
        class="original-page"
        :data-testid="`${kind}-page`"
    >
        <PageHeading
            :title="resource?.name ?? 'Loading…'"
            :description="resource?.description"
            ><template v-if="resource"
                ><span
                    class="status-badge"
                    :class="{ warning: !resource.status?.startsWith('running') }"
                    >{{ resource.status }}</span
                ><a
                    v-if="kind === 'application'"
                    :href="`${base}/deployment`"
                    class="coolify-btn"
                    >Deployments</a
                ><template v-if="canDeploy"
                    ><button
                        type="button"
                        class="coolify-btn coolify-btn-primary"
                        :disabled="busy"
                        @click="action(kind === 'application' ? 'deploy' : 'start')"
                    >
                        {{ kind === 'application' ? 'Deploy' : 'Start' }}</button
                    ><button
                        type="button"
                        class="coolify-btn"
                        :disabled="busy"
                        @click="action('restart')"
                    >
                        Restart</button
                    ><button
                        type="button"
                        class="coolify-btn"
                        :disabled="busy"
                        @click="action('stop')"
                    >
                        Stop
                    </button></template
                ></template
            ></PageHeading
        >
        <nav
            class="resource-tabs"
            aria-label="Resource views"
        >
            <a
                :href="base"
                :aria-current="
                    !['deployment', 'logs', 'terminal'].includes(section) ? 'page' : undefined
                "
                >Configuration</a
            ><a
                v-if="kind === 'application'"
                :href="`${base}/deployment`"
                :aria-current="section === 'deployment' ? 'page' : undefined"
                >Deployments</a
            ><a
                :href="`${base}/logs`"
                :aria-current="section === 'logs' ? 'page' : undefined"
                >Logs</a
            ><a
                :href="`${base}/terminal`"
                :aria-current="section === 'terminal' ? 'page' : undefined"
                >Terminal</a
            >
        </nav>
        <p
            v-if="error"
            role="alert"
            class="page-error"
        >
            {{ error }}
        </p>
        <p
            v-if="status"
            role="status"
        >
            {{ status }}
        </p>
        <div
            v-if="resource"
            class="settings-workspace"
        >
            <nav
                class="settings-sidebar"
                aria-label="Resource configuration sections"
            >
                <a
                    v-for="[label, path, icon] in menu"
                    :key="path"
                    :href="`${base}${path === 'general' ? '' : `/${path}`}`"
                    :aria-current="section === path ? 'page' : undefined"
                    ><AppIcon
                        :name="icon === 'sliders' ? 'settings' : icon"
                        :size="14"
                    />{{ label }}</a
                >
            </nav>
            <div class="settings-content">
                <template v-if="section === 'general'"
                    ><form @submit.prevent="save">
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
                                :title="
                                    kind === 'service'
                                        ? 'Service details'
                                        : kind === 'application'
                                          ? 'Application details'
                                          : 'Database details'
                                "
                                ><template
                                    v-if="kind === 'service'"
                                    #actions
                                    ><button
                                        type="button"
                                        class="coolify-btn"
                                        :disabled="!canEdit"
                                        @click="composeDialog?.showModal()"
                                    >
                                        Edit Compose File
                                    </button></template
                                >
                                <div class="form-grid">
                                    <label class="form-field"
                                        >Name<input
                                            v-model="form.name"
                                            class="coolify-input"
                                            required
                                            :readonly="!canEdit || (kind === 'database' && resource.id === 0)" /></label
                                    ><label class="form-field"
                                        >Description<input
                                            v-model="form.description"
                                            class="coolify-input"
                                            :readonly="!canEdit || (kind === 'database' && resource.id === 0)"
                                    /></label></div></SettingsSection
                            ><SettingsSection
                                v-if="kind === 'application'"
                                title="Access"
                                ><div class="form-grid">
                                    <label class="form-field"
                                        >Domains<input
                                            v-model="form.fqdn"
                                            class="coolify-input"
                                            placeholder="https://app.example.com"
                                            :readonly="!canEdit" /></label
                                    ><label class="form-field"
                                        >Ports Exposes<input
                                            v-model="form.ports_exposes"
                                            class="coolify-input"
                                            placeholder="80"
                                            :readonly="!canEdit"
                                    /></label></div></SettingsSection
                            ><SettingsSection
                                v-if="kind === 'application'"
                                title="Build &amp; deploy"
                                ><div class="form-grid">
                                    <label class="form-field"
                                        >Build Pack<select
                                            v-model="form.build_pack"
                                            class="coolify-input"
                                            :disabled="!canEdit"
                                        >
                                            <option value="dockerfile">Dockerfile</option>
                                            <option value="railpack">Railpack</option>
                                            <option value="dockerimage">Docker Image</option>
                                            <option value="dockercompose">Docker Compose</option>
                                            <option value="static">Static</option>
                                        </select></label
                                    ><label class="form-field"
                                        >Branch<input
                                            v-model="form.git_branch"
                                            class="coolify-input"
                                            :readonly="!canEdit" /></label
                                    ><label class="form-field"
                                        >Base Directory<input
                                            v-model="form.base_directory"
                                            class="coolify-input"
                                            placeholder="/"
                                            :readonly="!canEdit" /></label
                                    ><label
                                        v-if="form.build_pack === 'static'"
                                        class="form-field"
                                        >Publish Directory<input
                                            v-model="form.publish_directory"
                                            class="coolify-input"
                                            placeholder="/dist"
                                            :readonly="!canEdit" /></label
                                    ><label
                                        v-if="form.build_pack === 'dockerfile'"
                                        class="form-field"
                                        >Dockerfile Location<input
                                            v-model="form.dockerfile_location"
                                            class="coolify-input"
                                            placeholder="/Dockerfile"
                                            :readonly="!canEdit" /></label
                                    ><template v-if="form.build_pack === 'dockerimage'"
                                        ><label class="form-field"
                                            >Docker Image<input
                                                v-model="form.docker_registry_image_name"
                                                class="coolify-input"
                                                :readonly="!canEdit" /></label
                                        ><label class="form-field"
                                            >Tag<input
                                                v-model="form.docker_registry_image_tag"
                                                class="coolify-input"
                                                :readonly="!canEdit" /></label></template
                                    ><label
                                        v-if="form.build_pack === 'dockerfile' && !resource.git_repository"
                                        class="form-field full-span"
                                        >Dockerfile<textarea
                                            v-model="form.dockerfile"
                                            class="coolify-input code-editor"
                                            rows="10"
                                            :readonly="!canEdit" /></label
                                    ><label
                                        v-if="form.build_pack === 'dockercompose'"
                                        class="form-field full-span"
                                        >Docker Compose<textarea
                                            v-model="form.docker_compose_raw"
                                            class="coolify-input code-editor"
                                            rows="10"
                                            :readonly="!canEdit"
                                    /></label></div></SettingsSection
                            ><SettingsSection
                                v-if="kind === 'database'"
                                title="Network &amp; Credentials"
                                ><template #actions
                                    ><button
                                        v-if="canEdit && resource.id !== 0"
                                        type="button"
                                        class="coolify-btn"
                                        :disabled="busy"
                                        @click="rotateDatabasePassword"
                                    >
                                        Rotate Password
                                    </button></template
                                >
                                <div class="form-grid">
                                    <label class="form-field full-span"
                                        >Docker Image<input
                                            v-model="form.image"
                                            class="coolify-input"
                                            :readonly="!canEdit || resource.id === 0" /></label
                                    ><label class="check-field full-span"
                                        ><input
                                            v-model="form.is_public"
                                            type="checkbox"
                                            :disabled="!canEdit || resource.id === 0"
                                        />Make it publicly available</label
                                    ><label class="form-field"
                                        >Public port<input
                                            v-model.number="form.public_port"
                                            type="number"
                                            min="0"
                                            max="65535"
                                            class="coolify-input"
                                            :readonly="!canEdit || resource.id === 0" /></label
                                    ><div
                                        v-if="resource.internal_db_url"
                                        class="form-field full-span"
                                    >
                                        <span>Internal DB URL</span>
                                        <SecretInput
                                            :value="resource.internal_db_url"
                                            label="Internal DB URL"
                                        />
                                    </div>
                                    <div
                                        v-if="resource.external_db_url"
                                        class="form-field full-span"
                                    >
                                        <span>External DB URL</span>
                                        <SecretInput
                                            :value="resource.external_db_url"
                                            label="External DB URL"
                                        />
                                    </div></div
                            ></SettingsSection>
                        </div>
                    </form>
                    <SettingsSection
                        v-if="kind === 'service'"
                        title="Services"
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
                                        v-for="row in rows"
                                        :key="row.uuid"
                                    >
                                        <td>
                                            <a :href="`${base}/${row.kind}/${row.uuid}`">{{
                                                row.name
                                            }}</a>
                                        </td>
                                        <td>{{ row.kind }}</td>
                                        <td>{{ row.status }}</td>
                                    </tr>
                                </tbody>
                            </table>
                        </div></SettingsSection
                    ></template
                >
                <SettingsSection
                    v-else-if="section === 'environment-variables'"
                    title="Environment Variables"
                    ><template #actions
                        ><button
                            v-if="canEdit"
                            type="button"
                            class="coolify-btn coolify-btn-primary"
                            @click="envDialog?.showModal()"
                        >
                            <AppIcon name="plus" />Add
                        </button></template
                    >
                    <div class="original-table-scroll">
                        <table class="original-table">
                            <thead>
                                <tr>
                                    <th>Name</th>
                                    <th>Value</th>
                                    <th>Actions</th>
                                </tr>
                            </thead>
                            <tbody>
                                <tr
                                    v-for="row in rows"
                                    :key="row.uuid"
                                >
                                    <td class="code-text">{{ row.key }}</td>
                                    <td>
                                        <SecretInput
                                            :value="row.value ?? ''"
                                            :label="`${row.key} value`"
                                        />
                                    </td>
                                    <td>
                                        <button
                                            v-if="canEdit"
                                            type="button"
                                            class="coolify-btn danger-button"
                                            @click="removeEnv(row.uuid)"
                                        >
                                            Delete
                                        </button>
                                    </td>
                                </tr>
                            </tbody>
                        </table>
                    </div>
                    <p
                        v-if="!rows.length"
                        class="page-muted"
                    >
                        No environment variables found
                    </p></SettingsSection
                >
                <DeploymentHistory
                    v-else-if="section === 'deployment'"
                    :application-uuid="uuid"
                />
                <ScheduleList
                    v-else-if="section === 'scheduled-tasks'"
                    :application-uuid="kind === 'application' ? uuid : undefined"
                    :service-uuid="kind === 'service' ? uuid : undefined"
                />
                <BackupManager
                    v-else-if="section === 'backups' && kind === 'database'"
                    :database-uuid="uuid"
                />
                <LogViewer
                    v-else-if="section === 'logs'"
                    :resource-uuid="uuid"
                />
                <WebTerminal
                    v-else-if="section === 'terminal' && resource.server_uuid"
                    :server-uuid="resource.server_uuid"
                />
                <form
                    v-else-if="section === 'source' && kind === 'application'"
                    @submit.prevent="save"
                >
                    <SettingsSection title="Source"
                        ><template #actions
                            ><button
                                v-if="canEdit"
                                type="submit"
                                class="coolify-btn coolify-btn-primary"
                                :disabled="busy || !dirty"
                            >
                                Save
                            </button></template
                        >
                        <div class="form-grid">
                            <label class="form-field full-span"
                                >Repository<input
                                    v-model="form.git_repository"
                                    class="coolify-input"
                                    :readonly="!canEdit" /></label
                            ><label class="form-field"
                                >Branch<input
                                    v-model="form.git_branch"
                                    class="coolify-input"
                                    :readonly="!canEdit" /></label
                            ><label class="form-field"
                                >Deploy Key<select
                                    v-model="form.private_key_uuid"
                                    class="coolify-input"
                                    :disabled="!canEdit"
                                >
                                    <option value="">None (public / server default)</option>
                                    <option
                                        v-for="key in keys"
                                        :key="key.uuid"
                                        :value="key.uuid"
                                    >
                                        {{ key.name ?? key.uuid }}
                                    </option>
                                </select></label
                            >
                        </div></SettingsSection
                    >
                </form>
                <form
                    v-else-if="section === 'advanced' && kind === 'application'"
                    @submit.prevent="save"
                >
                    <SettingsSection title="Advanced"
                        ><template #actions
                            ><button
                                v-if="canEdit"
                                type="submit"
                                class="coolify-btn coolify-btn-primary"
                                :disabled="busy || !dirty"
                            >
                                Save
                            </button></template
                        >
                        <div class="form-grid">
                            <label
                                v-for="[key, label] in advancedFlags"
                                :key="key"
                                class="check-field"
                                ><input
                                    v-model="advancedSettings[key]"
                                    type="checkbox"
                                    :disabled="!canEdit"
                                />{{ label }}</label
                            >
                            <label class="form-field full-span"
                                >Custom Docker Options<input
                                    v-model="form.custom_docker_run_options"
                                    class="coolify-input"
                                    placeholder="--cap-add SYS_ADMIN"
                                    :readonly="!canEdit" /></label
                            ><label class="form-field full-span"
                                >Custom Container Labels (KEY=VALUE per line)<textarea
                                    v-model="form.custom_labels"
                                    class="coolify-input code-editor"
                                    rows="5"
                                    :readonly="!canEdit"
                            /></label>
                        </div></SettingsSection
                    >
                </form>
                <SettingsSection
                    v-else-if="section === 'storage'"
                    title="Persistent Storage"
                    ><template #actions
                        ><button
                            v-if="canEdit"
                            type="button"
                            class="coolify-btn coolify-btn-primary"
                            @click="storageDialog?.showModal()"
                        >
                            <AppIcon name="plus" />Add
                        </button></template
                    >
                    <div class="original-table-scroll">
                        <table class="original-table">
                            <thead>
                                <tr>
                                    <th>Type</th>
                                    <th>Source / Name</th>
                                    <th>Mount Path</th>
                                    <th>Actions</th>
                                </tr>
                            </thead>
                            <tbody>
                                <tr
                                    v-for="vol in storages.persistent_storages"
                                    :key="vol.uuid"
                                >
                                    <td>Volume</td>
                                    <td class="code-text">{{ vol.host_path || vol.name }}</td>
                                    <td class="code-text">{{ vol.mount_path }}</td>
                                    <td>
                                        <button
                                            v-if="canEdit"
                                            type="button"
                                            class="coolify-btn danger-button"
                                            @click="removeStorage(vol.uuid)"
                                        >
                                            Delete
                                        </button>
                                    </td>
                                </tr>
                                <tr
                                    v-for="file in storages.file_storages"
                                    :key="file.uuid"
                                >
                                    <td>File</td>
                                    <td class="code-text">{{ file.fs_path }}</td>
                                    <td class="code-text">{{ file.mount_path }}</td>
                                    <td>
                                        <button
                                            v-if="canEdit"
                                            type="button"
                                            class="coolify-btn danger-button"
                                            @click="removeStorage(file.uuid)"
                                        >
                                            Delete
                                        </button>
                                    </td>
                                </tr>
                            </tbody>
                        </table>
                    </div>
                    <p
                        v-if="!storages.persistent_storages.length && !storages.file_storages.length"
                        class="page-muted"
                    >
                        No persistent volumes or file mounts configured
                    </p></SettingsSection
                >
                <form
                    v-else-if="section === 'healthcheck' && kind === 'application'"
                    @submit.prevent="save"
                >
                    <SettingsSection title="Healthcheck"
                        ><template #actions
                            ><button
                                v-if="canEdit"
                                type="submit"
                                class="coolify-btn coolify-btn-primary"
                                :disabled="busy || !dirty"
                            >
                                Save
                            </button></template
                        >
                        <div class="form-grid">
                            <label class="check-field full-span"
                                ><input
                                    v-model="form.health_check_enabled"
                                    type="checkbox"
                                    :disabled="!canEdit"
                                />Enable Healthcheck</label
                            >
                            <label class="form-field"
                                >Method<select
                                    v-model="form.health_check_method"
                                    class="coolify-input"
                                    :disabled="!canEdit"
                                >
                                    <option value="GET">GET</option>
                                    <option value="HEAD">HEAD</option>
                                </select></label
                            ><label class="form-field"
                                >Scheme<select
                                    v-model="form.health_check_scheme"
                                    class="coolify-input"
                                    :disabled="!canEdit"
                                >
                                    <option value="http">http</option>
                                    <option value="https">https</option>
                                </select></label
                            ><label class="form-field"
                                >Host<input
                                    v-model="form.health_check_host"
                                    class="coolify-input"
                                    :readonly="!canEdit" /></label
                            ><label class="form-field"
                                >Port<input
                                    v-model="form.health_check_port"
                                    class="coolify-input"
                                    placeholder="Uses exposed port"
                                    :readonly="!canEdit" /></label
                            ><label class="form-field"
                                >Path<input
                                    v-model="form.health_check_path"
                                    class="coolify-input"
                                    :readonly="!canEdit" /></label
                            ><label class="form-field"
                                >Return Code<input
                                    v-model.number="form.health_check_return_code"
                                    type="number"
                                    class="coolify-input"
                                    :readonly="!canEdit" /></label
                            ><label class="form-field"
                                >Interval (s)<input
                                    v-model.number="form.health_check_interval"
                                    type="number"
                                    min="1"
                                    class="coolify-input"
                                    :readonly="!canEdit" /></label
                            ><label class="form-field"
                                >Timeout (s)<input
                                    v-model.number="form.health_check_timeout"
                                    type="number"
                                    min="1"
                                    class="coolify-input"
                                    :readonly="!canEdit" /></label
                            ><label class="form-field"
                                >Retries<input
                                    v-model.number="form.health_check_retries"
                                    type="number"
                                    min="1"
                                    class="coolify-input"
                                    :readonly="!canEdit" /></label
                            ><label class="form-field"
                                >Start Period (s)<input
                                    v-model.number="form.health_check_start_period"
                                    type="number"
                                    min="0"
                                    class="coolify-input"
                                    :readonly="!canEdit"
                            /></label>
                        </div></SettingsSection
                    >
                </form>
                <form
                    v-else-if="section === 'resource-limits' && kind === 'application'"
                    @submit.prevent="save"
                >
                    <SettingsSection title="Resource Limits"
                        ><template #actions
                            ><button
                                v-if="canEdit"
                                type="submit"
                                class="coolify-btn coolify-btn-primary"
                                :disabled="busy || !dirty"
                            >
                                Save
                            </button></template
                        >
                        <div class="form-grid">
                            <label class="form-field"
                                >Memory Limit (e.g. 512m, 2g, 0 = unlimited)<input
                                    v-model="form.limits_memory"
                                    class="coolify-input"
                                    :readonly="!canEdit" /></label
                            ><label class="form-field"
                                >Memory Swap<input
                                    v-model="form.limits_memory_swap"
                                    class="coolify-input"
                                    :readonly="!canEdit" /></label
                            ><label class="form-field"
                                >Memory Reservation<input
                                    v-model="form.limits_memory_reservation"
                                    class="coolify-input"
                                    :readonly="!canEdit" /></label
                            ><label class="form-field"
                                >Memory Swappiness (0-100)<input
                                    v-model.number="form.limits_memory_swappiness"
                                    type="number"
                                    min="0"
                                    max="100"
                                    class="coolify-input"
                                    :readonly="!canEdit" /></label
                            ><label class="form-field"
                                >CPUs (e.g. 0.5, 2, 0 = unlimited)<input
                                    v-model="form.limits_cpus"
                                    class="coolify-input"
                                    :readonly="!canEdit" /></label
                            ><label class="form-field"
                                >CPU Set (e.g. 0-1)<input
                                    v-model="form.limits_cpuset"
                                    class="coolify-input"
                                    :readonly="!canEdit" /></label
                            ><label class="form-field"
                                >CPU Shares<input
                                    v-model.number="form.limits_cpu_shares"
                                    type="number"
                                    min="2"
                                    class="coolify-input"
                                    :readonly="!canEdit"
                            /></label>
                        </div></SettingsSection
                    >
                </form>
                <SettingsSection
                    v-else-if="section === 'rollback' && kind === 'application'"
                    title="Rollback"
                    ><div class="original-table-scroll">
                        <table class="original-table">
                            <thead>
                                <tr>
                                    <th>Deployment</th>
                                    <th>Status</th>
                                    <th>Created</th>
                                    <th>Action</th>
                                </tr>
                            </thead>
                            <tbody>
                                <tr
                                    v-for="dep in deployments"
                                    :key="dep.deployment_uuid"
                                >
                                    <td class="code-text">
                                        <a :href="`${base}/deployment/${dep.deployment_uuid}`">{{
                                            dep.deployment_uuid
                                        }}</a>
                                    </td>
                                    <td>{{ dep.status }}</td>
                                    <td>{{ dep.created_at }}</td>
                                    <td>
                                        <button
                                            v-if="canDeploy"
                                            type="button"
                                            class="coolify-btn"
                                            :disabled="busy"
                                            @click="rollbackTo(dep.deployment_uuid)"
                                        >
                                            Rollback
                                        </button>
                                    </td>
                                </tr>
                            </tbody>
                        </table>
                    </div>
                    <p
                        v-if="!deployments.length"
                        class="page-muted"
                    >
                        No deployments available for rollback
                    </p></SettingsSection
                >
                <SettingsSection
                    v-else-if="section === 'servers' && kind === 'application'"
                    title="Servers &amp; Destinations"
                    ><div class="original-table-scroll">
                        <table class="original-table">
                            <thead>
                                <tr>
                                    <th>Role</th>
                                    <th>Server</th>
                                    <th>Destination</th>
                                    <th>Network</th>
                                    <th>Action</th>
                                </tr>
                            </thead>
                            <tbody>
                                <tr
                                    v-for="dest in attachedDestinations"
                                    :key="dest.uuid"
                                >
                                    <td>{{ dest.is_primary ? 'Primary' : 'Secondary' }}</td>
                                    <td>
                                        <a :href="`/server/${dest.server_uuid}`">{{
                                            dest.server_uuid
                                        }}</a>
                                    </td>
                                    <td>{{ dest.name }}</td>
                                    <td class="code-text">{{ dest.network }}</td>
                                    <td>
                                        <button
                                            v-if="!dest.is_primary && canEdit"
                                            type="button"
                                            class="coolify-btn danger-button"
                                            :disabled="busy"
                                            @click="detachDestination(dest.uuid)"
                                        >
                                            Detach
                                        </button>
                                        <span
                                            v-else
                                            class="page-muted"
                                            >-</span
                                        >
                                    </td>
                                </tr>
                            </tbody>
                        </table>
                    </div>
                    <form
                        v-if="canEdit"
                        class="form-grid"
                        style="margin-top: 14px;"
                        @submit.prevent="attachDestination"
                    >
                        <label class="form-field"
                            >Attach Secondary Server / Destination<select
                                v-model="selectedDestinationUuid"
                                class="coolify-input"
                            >
                                <option value="">Select destination...</option>
                                <option
                                    v-for="d in allDestinations"
                                    :key="d.uuid"
                                    :value="d.uuid"
                                >
                                    {{ d.name }} ({{ d.server_uuid }} / {{ d.network }})
                                </option>
                            </select></label
                        >
                        <div class="page-actions" style="align-items: end;">
                            <button
                                type="submit"
                                class="coolify-btn coolify-btn-primary"
                                :disabled="busy || !selectedDestinationUuid"
                            >
                                Attach
                            </button>
                        </div>
                    </form></SettingsSection
                >
                <SettingsSection
                    v-else-if="section === 'metrics'"
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
                    <MetricsChart
                        v-if="serverMetrics.has_metrics && serverMetrics.history.length"
                        :history="serverMetrics.history"
                    />
                    <p
                        v-else
                        class="page-muted"
                    >
                        No metrics available yet for this server.
                    </p></SettingsSection
                >
                <SettingsSection
                    v-else-if="section === 'danger'"
                    title="Danger zone"
                    ><div class="danger-row">
                        <div>
                            <strong>Delete {{ kind }}</strong>
                            <p>
                                This permanently removes the resource. This action cannot be undone.
                            </p>
                        </div>
                        <button
                            type="button"
                            class="coolify-btn danger-button"
                            :disabled="
                                !permissions.delete || (kind === 'database' && resource.id === 0)
                            "
                            @click="deleteDialog?.showModal()"
                        >
                            Delete
                        </button>
                    </div></SettingsSection
                >
                <SettingsSection
                    v-else-if="section === 'tags' || section === 'previews'"
                    :title="section === 'tags' ? 'Tags' : 'Previews'"
                    ><div
                        v-for="row in rows"
                        :key="row.uuid ?? row.id"
                    >
                        {{ row.name ?? row.pull_request_id }}
                    </div>
                    <p
                        v-if="!rows.length"
                        class="page-muted"
                    >
                        No {{ section }} found
                    </p></SettingsSection
                >
                <SettingsSection
                    v-else
                    :title="menu.find((m) => m[1] === section)?.[0] ?? section"
                    ><p
                        role="alert"
                        class="page-error"
                    >
                        This page has not been restored yet.
                    </p></SettingsSection
                >
            </div>
        </div>
        <dialog
            ref="storageDialog"
            class="workspace-dialog"
        >
            <form
                class="dialog-form"
                @submit.prevent="addStorage"
            >
                <div class="dialog-heading">
                    <h2>Add Persistent Storage</h2>
                    <button
                        type="button"
                        class="chrome-button"
                        aria-label="Close"
                        @click="storageDialog?.close()"
                    >
                        ✕
                    </button>
                </div>
                <label class="form-field"
                    >Type<select
                        v-model="storageForm.type"
                        class="coolify-input"
                    >
                        <option value="persistent">Persistent Volume</option>
                        <option value="file">File Mount</option>
                    </select></label
                >
                <template v-if="storageForm.type === 'persistent'">
                    <label class="form-field"
                        >Volume Name<input
                            v-model="storageForm.name"
                            class="coolify-input"
                            placeholder="app-data"
                            required /></label
                    ><label class="form-field"
                        >Host Path (optional)<input
                            v-model="storageForm.host_path"
                            class="coolify-input"
                            placeholder="/data/app"
                    /></label>
                </template>
                <template v-else>
                    <label class="form-field"
                        >Host File Path<input
                            v-model="storageForm.fs_path"
                            class="coolify-input"
                            placeholder="/data/coolify/config.json"
                            required /></label
                    ><label class="form-field"
                        >File Content<textarea
                            v-model="storageForm.content"
                            class="coolify-input code-editor"
                            rows="5"
                    /></label>
                </template>
                <label class="form-field"
                    >Destination Mount Path<input
                        v-model="storageForm.mount_path"
                        class="coolify-input"
                        placeholder="/app/data"
                        required
                /></label>
                <div class="dialog-actions">
                    <button
                        type="button"
                        class="coolify-btn"
                        @click="storageDialog?.close()"
                    >
                        Cancel</button
                    ><button
                        type="submit"
                        class="coolify-btn coolify-btn-primary"
                        :disabled="busy"
                    >
                        Add
                    </button>
                </div>
            </form>
        </dialog>
        <dialog
            ref="envDialog"
            class="workspace-dialog"
        >
            <form
                class="dialog-form"
                @submit.prevent="addEnv"
            >
                <div class="dialog-heading">
                    <h2>New Environment Variable</h2>
                    <button
                        type="button"
                        class="chrome-button"
                        aria-label="Close"
                        @click="envDialog?.close()"
                    >
                        ✕
                    </button>
                </div>
                <label class="form-field"
                    >Name<input
                        v-model="envKey"
                        class="coolify-input"
                        required /></label
                ><label class="form-field"
                    >Value<textarea
                        v-model="envValue"
                        class="coolify-input"
                        rows="4"
                    /></label
                ><label class="check-field"
                    ><input
                        v-model="envPreview"
                        type="checkbox"
                    />Preview</label
                >
                <p
                    v-if="error"
                    role="alert"
                    class="page-error"
                >
                    {{ error }}
                </p>
                <button
                    type="submit"
                    class="coolify-btn coolify-btn-primary"
                    :disabled="busy"
                >
                    Save
                </button>
            </form>
        </dialog>
        <dialog
            ref="composeDialog"
            class="workspace-dialog wide-dialog"
        >
            <form
                class="dialog-form"
                @submit.prevent="save"
            >
                <div class="dialog-heading">
                    <h2>Edit Compose File</h2>
                    <button
                        type="button"
                        class="chrome-button"
                        aria-label="Close"
                        @click="composeDialog?.close()"
                    >
                        ✕
                    </button>
                </div>
                <label class="form-field"
                    >Docker Compose<textarea
                        v-model="form.docker_compose_raw"
                        class="coolify-input code-editor"
                        rows="18"
                        required
                    />
                </label>
                <p
                    v-if="error"
                    role="alert"
                    class="page-error"
                >
                    {{ error }}
                </p>
                <button
                    type="submit"
                    class="coolify-btn coolify-btn-primary"
                    :disabled="busy"
                >
                    Save
                </button>
            </form>
        </dialog>
        <dialog
            ref="deleteDialog"
            class="workspace-dialog"
        >
            <form
                class="dialog-form"
                @submit.prevent="remove"
            >
                <h2>Confirm deletion?</h2>
                <label class="form-field"
                    >Confirm by entering {{ resource?.name
                    }}<input
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
                        @click="deleteDialog?.close()"
                    >
                        Cancel</button
                    ><button
                        type="submit"
                        class="coolify-btn danger-button"
                        :disabled="!permissions.delete || busy || confirmation !== resource?.name"
                    >
                        Permanently Delete
                    </button>
                </div>
            </form>
        </dialog>
    </section>
</template>
