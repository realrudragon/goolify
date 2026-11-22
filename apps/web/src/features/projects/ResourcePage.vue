<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import PageHeading from '../../components/PageHeading.vue';
import SettingsSection from '../../components/SettingsSection.vue';
import AppIcon from '../../components/AppIcon.vue';
import DeploymentHistory from '../deployments/DeploymentHistory.vue';
import ScheduleList from '../schedules/ScheduleList.vue';
import BackupManager from '../backups/BackupManager.vue';
import LogViewer from '../logs/LogViewer.vue';
import WebTerminal from '../terminal/WebTerminal.vue';
import { apiRequest } from '../../lib/api';
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
const composeDialog = ref<HTMLDialogElement>();
const deleteDialog = ref<HTMLDialogElement>();
const confirmation = ref('');
const endpoint = computed(
    () =>
        `/api/v1/${props.kind === 'application' ? 'applications' : props.kind === 'service' ? 'services' : 'databases'}/${encodeURIComponent(props.uuid)}`,
);
const base = computed(
    () =>
        `/project/${props.projectUuid ?? resource.value?.project_uuid}/environment/${props.environmentUuid ?? resource.value?.environment_uuid}/${props.kind}/${props.uuid}`,
);
const section = computed(() => props.subpage.split('/')[0]);
const dirty = computed(() => JSON.stringify(form.value) !== initial.value);
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
        form.value =
            props.kind === 'application'
                ? {
                      name: item.name,
                      description: item.description ?? '',
                      fqdn: item.fqdn ?? '',
                      git_branch: item.git_branch ?? '',
                  }
                : props.kind === 'service'
                  ? {
                        name: item.name,
                        description: item.description ?? '',
                        docker_compose_raw: item.docker_compose_raw ?? '',
                    }
                  : { custom_port: item.custom_port ?? 0 };
        initial.value = JSON.stringify(form.value);
        if (section.value === 'environment-variables')
            rows.value = await apiRequest(`${endpoint.value}/envs`, { signal: lifecycle.signal });
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
        await apiRequest(endpoint.value, { method: 'PATCH', body: JSON.stringify(form.value) });
        composeDialog.value?.close();
        await load();
        status.value = 'Saved.';
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
                                            v-if="kind !== 'database'"
                                            v-model="form.name"
                                            class="coolify-input"
                                            required
                                            :readonly="!canEdit" /><input
                                            v-else
                                            :value="resource.name"
                                            class="coolify-input"
                                            readonly /></label
                                    ><label class="form-field"
                                        >Description<input
                                            v-if="kind !== 'database'"
                                            v-model="form.description"
                                            class="coolify-input"
                                            :readonly="!canEdit" /><input
                                            v-else
                                            :value="resource.description"
                                            class="coolify-input"
                                            readonly /></label
                                    ><label
                                        v-if="kind === 'database'"
                                        class="form-field full-span"
                                        >Image<input
                                            :value="resource.image"
                                            class="coolify-input"
                                            readonly
                                    /></label></div></SettingsSection
                            ><SettingsSection
                                v-if="kind === 'application'"
                                title="Access"
                                ><label class="form-field"
                                    >Domains<input
                                        v-model="form.fqdn"
                                        class="coolify-input"
                                        :readonly="!canEdit" /></label></SettingsSection
                            ><SettingsSection
                                v-if="kind === 'application'"
                                title="Build &amp; deploy"
                                ><div class="form-grid">
                                    <label class="form-field"
                                        >Build Pack<input
                                            :value="resource.build_pack"
                                            class="coolify-input"
                                            readonly /></label
                                    ><label class="form-field"
                                        >Branch<input
                                            v-model="form.git_branch"
                                            class="coolify-input"
                                            :readonly="!canEdit"
                                    /></label></div></SettingsSection
                            ><SettingsSection
                                v-if="kind === 'database'"
                                title="Network"
                                ><div class="form-grid">
                                    <label class="form-field"
                                        >Public port<input
                                            v-model.number="form.custom_port"
                                            type="number"
                                            min="0"
                                            max="65535"
                                            class="coolify-input"
                                            :readonly="!canEdit"
                                    /></label></div
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
                                        <input
                                            type="password"
                                            :value="row.value"
                                            readonly
                                            :aria-label="`${row.key} value`"
                                            class="coolify-input"
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
                <SettingsSection
                    v-else-if="section === 'source'"
                    title="Source"
                    ><div class="form-grid">
                        <label class="form-field"
                            >Repository<input
                                :value="resource.git_repository"
                                class="coolify-input"
                                readonly /></label
                        ><label class="form-field"
                            >Branch<input
                                :value="resource.git_branch"
                                class="coolify-input"
                                readonly
                        /></label></div
                ></SettingsSection>
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
