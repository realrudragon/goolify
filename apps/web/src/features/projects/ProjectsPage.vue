<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue';
import PageHeading from '../../components/PageHeading.vue';
import SettingsSection from '../../components/SettingsSection.vue';
import AppIcon from '../../components/AppIcon.vue';
import ResourceCreate from './ResourceCreate.vue';
import CanvasGraph from '../canvas/CanvasGraph.vue';
import { apiRequest } from '../../lib/api';
import { navigateTo } from '../../lib/navigation';
import { session } from '../../lib/session';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const props = defineProps<{ projectUuid?: string; environmentUuid?: string; subpage?: string }>();
interface Project {
    id: number;
    uuid: string;
    name: string;
    description?: string;
    team_id: number;
}
interface Environment {
    id: number;
    uuid: string;
    name: string;
    description?: string;
    project_id: number;
}
interface Resource {
    uuid: string;
    name: string;
    status: string;
    description?: string;
    type: string;
    project_uuid?: string;
    environment_uuid?: string;
    environment_id: number;
    server_uuid?: string;
    fqdn?: string;
}
const lifecycle = usePageLifecycle();
const projects = ref<Project[]>([]);
const project = ref<Project>();
const environments = ref<Environment[]>([]);
const allEnvironments = ref<Environment[]>([]);
const environment = ref<Environment>();
const resources = ref<Resource[]>([]);
const search = ref('');
const typeFilter = ref('');
const statusFilter = ref('');
const view = ref<'table' | 'grid' | 'canvas'>('table');
const ascending = ref(true);
const loading = ref(true);
const error = ref('');
const status = ref('');
const saving = ref(false);
const dialog = ref<HTMLDialogElement>();
const deleteDialog = ref<HTMLDialogElement>();
const confirmation = ref('');
const name = ref('');
const description = ref('');
const title = computed(() => environment.value?.name ?? project.value?.name ?? 'Projects');
const scope = computed(() =>
    props.environmentUuid ? 'resources' : props.projectUuid ? 'environments' : 'projects',
);
const base = computed(() =>
    props.environmentUuid
        ? `/project/${props.projectUuid}/environment/${props.environmentUuid}`
        : `/project/${props.projectUuid}`,
);
const canEdit = computed(() =>
    ['admin', 'owner'].includes(
        session.me?.teams
            .find((t) => t.id === (project.value?.team_id ?? session.me?.session_team_id))
            ?.role?.toLowerCase() ?? '',
    ),
);
const filtered = computed(() => {
    const list = props.environmentUuid
        ? resources.value
              .filter(
                  (r) =>
                      r.environment_uuid === props.environmentUuid ||
                      r.environment_id === environment.value?.id,
              )
              .filter((r) => !typeFilter.value || typeFilter.value === r.type)
              .filter((r) => !statusFilter.value || r.status.startsWith(statusFilter.value))
        : props.projectUuid
          ? environments.value
          : projects.value;
    return [...list]
        .filter((item) =>
            [item.name, item.description].some((v) =>
                v?.toLowerCase().includes(search.value.trim().toLowerCase()),
            ),
        )
        .sort((a, b) => a.name.localeCompare(b.name) * (ascending.value ? 1 : -1));
});
const hasResources = computed(() =>
    props.environmentUuid
        ? resources.value.some(
              (r) =>
                  r.environment_uuid === props.environmentUuid ||
                  r.environment_id === environment.value?.id,
          )
        : resources.value.some(
              (r) =>
                  r.project_uuid === props.projectUuid ||
                  environments.value.some((e) => e.id === r.environment_id),
          ),
);
function envCount(id: number) {
    return allEnvironments.value.filter((e) => e.project_id === id).length;
}
function resourceCount(item: Project | Environment | Resource) {
    return resources.value.filter((r) =>
        'project_id' in item
            ? r.environment_id === item.id
            : r.project_uuid === item.uuid ||
              allEnvironments.value.some(
                  (e) =>
                      e.project_id === ('id' in item ? item.id : -1) && r.environment_id === e.id,
              ),
    ).length;
}
function itemHref(item: Project | Environment | Resource) {
    if (props.environmentUuid) return `${base.value}/${(item as Resource).type}/${item.uuid}`;
    if (props.projectUuid) return `/project/${props.projectUuid}/environment/${item.uuid}`;
    return `/project/${item.uuid}`;
}
async function load() {
    error.value = '';
    try {
        if (props.projectUuid) {
            const result = await apiRequest<{ project: Project; environments: Environment[] }>(
                `/api/v1/projects/${encodeURIComponent(props.projectUuid)}`,
                { signal: lifecycle.signal },
            );
            project.value = result.project;
            environments.value = result.environments;
            allEnvironments.value = result.environments;
            if (props.environmentUuid)
                environment.value = await apiRequest<Environment>(
                    `/api/v1/projects/${encodeURIComponent(props.projectUuid)}/${encodeURIComponent(props.environmentUuid)}`,
                    { signal: lifecycle.signal },
                );
        } else {
            projects.value = await apiRequest<Project[]>('/api/v1/projects', {
                signal: lifecycle.signal,
            });
            allEnvironments.value = (
                await Promise.all(
                    projects.value.map((p) =>
                        apiRequest<Environment[]>(`/api/v1/projects/${p.uuid}/environments`, {
                            signal: lifecycle.signal,
                        }),
                    ),
                )
            ).flat();
        }
        resources.value = (
            await Promise.all(
                ['applications', 'services', 'databases'].map(async (kind) =>
                    (
                        await apiRequest<Resource[]>(`/api/v1/${kind}`, {
                            signal: lifecycle.signal,
                        })
                    ).map((r) => ({
                        ...r,
                        type:
                            kind === 'applications'
                                ? 'application'
                                : kind === 'services'
                                  ? 'service'
                                  : 'database',
                    })),
                ),
            )
        ).flat();
        if (props.subpage === 'edit') {
            name.value = environment.value?.name ?? project.value?.name ?? '';
            description.value = environment.value?.description ?? project.value?.description ?? '';
        }
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        loading.value = false;
    }
}
function openCreate() {
    name.value = '';
    description.value = '';
    error.value = '';
    dialog.value?.showModal();
}
async function save() {
    if (!canEdit.value || saving.value) return;
    saving.value = true;
    error.value = '';
    try {
        const editing = props.subpage === 'edit';
        const endpoint = props.environmentUuid
            ? `/api/v1/projects/${props.projectUuid}/environments/${props.environmentUuid}`
            : props.projectUuid
              ? `/api/v1/projects/${props.projectUuid}${editing ? '' : '/environments'}`
              : '/api/v1/projects';
        const result = await apiRequest<any>(endpoint, {
            method: editing ? 'PATCH' : 'POST',
            body: JSON.stringify({ name: name.value, description: description.value }),
        });
        dialog.value?.close();
        if (!editing) {
            const next = props.projectUuid
                ? `/project/${props.projectUuid}/environment/${result.uuid}`
                : `/project/${result.project.uuid}`;
            navigateTo(next);
        } else {
            await load();
            status.value = 'Saved.';
        }
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        saving.value = false;
    }
}
async function remove() {
    if (!canEdit.value || confirmation.value !== title.value || hasResources.value || saving.value)
        return;
    saving.value = true;
    try {
        await apiRequest(
            `/api/v1/projects/${props.projectUuid}${props.environmentUuid ? `/environments/${props.environmentUuid}` : ''}`,
            { method: 'DELETE' },
        );
        navigateTo(
            props.environmentUuid ? `/project/${props.projectUuid}` : '/projects',
        );
    } catch (err) {
        error.value = (err as Error).message;
        saving.value = false;
    }
}
function setView(value: 'table' | 'grid' | 'canvas') {
    view.value = value;
    if (value !== 'canvas') localStorage.setItem(`coolify-${scope.value}-view`, value);
}
watch(
    () => [props.projectUuid, props.environmentUuid, props.subpage],
    () => {
        status.value = '';
        error.value = '';
        void load();
    },
);
onMounted(() => {
    view.value = localStorage.getItem(`coolify-${scope.value}-view`) === 'grid' ? 'grid' : 'table';
    void load();
});
</script>

<template>
    <section
        class="original-page"
        :data-testid="`${scope}-page`"
    >
        <PageHeading
            :title="props.subpage === 'new' ? 'New resource' : title"
            :description="
                project
                    ? environment
                        ? `${project.name} / ${environment.name}`
                        : project.description
                    : undefined
            "
            ><template v-if="canEdit"
                ><a
                    v-if="environment && props.subpage !== 'new'"
                    :href="`${base}/new`"
                    class="coolify-btn coolify-btn-primary"
                    ><AppIcon
                        name="plus"
                        :size="14"
                    />New resource</a
                ><button
                    v-else-if="props.subpage !== 'edit' && props.subpage !== 'new'"
                    class="coolify-btn coolify-btn-primary"
                    type="button"
                    @click="openCreate"
                >
                    <AppIcon
                        name="plus"
                        :size="14"
                    />{{ project ? 'New environment' : 'New project' }}</button
                ><a
                    v-if="project && props.subpage !== 'edit'"
                    :href="`${base}/edit`"
                    class="coolify-btn"
                    aria-label="Settings"
                    ><AppIcon
                        name="settings"
                        :size="14" /></a></template
        ></PageHeading>
        <p
            v-if="error"
            class="page-error"
            role="alert"
        >
            {{ error }}
        </p>
        <p
            v-if="status"
            role="status"
        >
            {{ status }}
        </p>
        <ResourceCreate
            v-if="props.subpage === 'new' && projectUuid && environmentUuid"
            :project-uuid="projectUuid"
            :environment-uuid="environmentUuid"
        />
        <form
            v-else-if="props.subpage === 'edit'"
            class="settings-content"
            @submit.prevent="save"
        >
            <SettingsSection :title="environment ? 'Environment details' : 'Project details'"
                ><template #actions
                    ><button
                        v-if="canEdit"
                        type="submit"
                        class="coolify-btn coolify-btn-primary"
                        :disabled="saving"
                    >
                        Save
                    </button></template
                >
                <div class="form-grid">
                    <label class="form-field"
                        >Name<input
                            v-model="name"
                            class="coolify-input"
                            required
                            :readonly="!canEdit" /></label
                    ><label class="form-field"
                        >Description<input
                            v-model="description"
                            class="coolify-input"
                            :readonly="!canEdit"
                    /></label></div></SettingsSection
            ><SettingsSection title="Danger zone"
                ><div class="danger-row">
                    <div>
                        <strong>Delete {{ environment ? 'environment' : 'project' }}</strong>
                        <p>
                            Delete all resources before deleting this
                            {{ environment ? 'environment' : 'project' }}.
                        </p>
                    </div>
                    <button
                        type="button"
                        class="coolify-btn danger-button"
                        :disabled="!canEdit || hasResources"
                        @click="deleteDialog?.showModal()"
                    >
                        Delete
                    </button>
                </div></SettingsSection
            >
        </form>
        <template v-else>
            <div class="list-controls">
                <div class="list-search">
                    <AppIcon
                        name="search"
                        :size="14"
                    /><input
                        v-model="search"
                        type="search"
                        :placeholder="`Search ${scope}`"
                        :aria-label="`Search ${scope}`"
                    />
                </div>
                <div class="page-actions">
                    <select
                        v-if="environment"
                        v-model="typeFilter"
                        class="coolify-input"
                        aria-label="Resource type"
                    >
                        <option value="">All types</option>
                        <option value="application">Applications</option>
                        <option value="database">Databases</option>
                        <option value="service">Services</option></select
                    ><select
                        v-if="environment"
                        v-model="statusFilter"
                        class="coolify-input"
                        aria-label="Resource status"
                    >
                        <option value="">All statuses</option>
                        <option value="running">Running</option>
                        <option value="exited">Stopped</option>
                    </select>
                    <div class="view-toggle">
                        <button
                            type="button"
                            :aria-pressed="view === 'table'"
                            aria-label="Table view"
                            @click="setView('table')"
                        >
                            <AppIcon name="layers" /></button
                        ><button
                            type="button"
                            :aria-pressed="view === 'grid'"
                            aria-label="Grid view"
                            @click="setView('grid')"
                        >
                            <AppIcon name="grid" /></button
                        ><button
                            type="button"
                            :aria-pressed="view === 'canvas'"
                            @click="setView('canvas')"
                        >
                            Canvas <span class="try-badge">Try it</span>
                        </button>
                    </div>
                </div>
            </div>
            <CanvasGraph v-if="view === 'canvas'" />
            <p
                v-else-if="loading"
                class="page-muted"
                role="status"
            >
                Loading {{ scope }}…
            </p>
            <template v-else-if="filtered.length"
                ><div
                    v-if="view === 'grid'"
                    class="original-card-grid"
                >
                    <a
                        v-for="item in filtered"
                        :key="item.uuid"
                        :href="itemHref(item)"
                        class="original-resource-card"
                        ><span class="resource-glyph"
                            ><AppIcon
                                :name="
                                    environment ? 'layers' : project ? 'layers' : 'projects'
                                " /></span
                        ><strong>{{ item.name }}</strong>
                        <p>{{ item.description }}</p>
                        <small>{{
                            environment
                                ? (item as Resource).status
                                : `${resourceCount(item)} resources`
                        }}</small></a
                    >
                </div>
                <div
                    v-else
                    class="original-table-scroll"
                >
                    <table class="original-table">
                        <thead>
                            <tr>
                                <th>
                                    <button
                                        type="button"
                                        @click="ascending = !ascending"
                                    >
                                        {{
                                            environment
                                                ? 'Resource'
                                                : project
                                                  ? 'Environment'
                                                  : 'Project'
                                        }}
                                        {{ ascending ? '⌃' : '⌄' }}
                                    </button>
                                </th>
                                <th>{{ environment ? 'Type' : 'Contents' }}</th>
                                <th>{{ environment ? 'Status' : 'Description' }}</th>
                                <th>Actions</th>
                            </tr>
                        </thead>
                        <tbody>
                            <tr
                                v-for="item in filtered"
                                :key="item.uuid"
                            >
                                <td>
                                    <a
                                        :href="itemHref(item)"
                                        class="resource-name"
                                        ><span class="resource-glyph"
                                            ><AppIcon
                                                :name="
                                                    environment
                                                        ? 'layers'
                                                        : project
                                                          ? 'layers'
                                                          : 'projects'
                                                " /></span
                                        ><strong>{{ item.name }}</strong></a
                                    >
                                </td>
                                <td>
                                    <div
                                        v-if="!environment"
                                        class="resource-counts"
                                    >
                                        <span v-if="!project"
                                            ><AppIcon
                                                name="layers"
                                                :size="14"
                                            />{{ envCount((item as Project).id) }}</span
                                        ><span
                                            ><AppIcon
                                                name="grid"
                                                :size="14"
                                            />{{ resourceCount(item) }}</span
                                        >
                                    </div>
                                    <span v-else>{{ (item as Resource).type }}</span>
                                </td>
                                <td>
                                    <span
                                        v-if="environment"
                                        class="status-label"
                                        ><i
                                            :class="
                                                (item as Resource).status.startsWith('running')
                                                    ? 'status-dot success'
                                                    : 'status-dot warning'
                                            "
                                        />{{ (item as Resource).status }}</span
                                    ><span v-else>{{ item.description || '-' }}</span>
                                </td>
                                <td>
                                    <div class="page-actions">
                                        <a
                                            :href="
                                                environment
                                                    ? itemHref(item)
                                                    : `${itemHref(item)}/new`
                                            "
                                            :aria-label="`Add resource to ${item.name}`"
                                            ><AppIcon name="plus" /></a
                                        ><a
                                            :href="
                                                environment
                                                    ? itemHref(item)
                                                    : `${itemHref(item)}/edit`
                                            "
                                            :aria-label="`Settings for ${item.name}`"
                                            ><AppIcon name="settings"
                                        /></a>
                                    </div>
                                </td>
                            </tr>
                        </tbody>
                    </table></div
            ></template>
            <div
                v-else-if="!error"
                class="original-empty"
            >
                <AppIcon
                    :name="environment ? 'layers' : 'projects'"
                    :size="32"
                />
                <h2>No {{ scope }}{{ search ? ' match your search' : ' yet' }}</h2>
                <p v-if="!search">
                    {{
                        environment
                            ? 'Add a resource to get started.'
                            : project
                              ? 'Create an environment to organize your resources.'
                              : 'Create a project to get started.'
                    }}
                </p>
            </div>
        </template>
        <dialog
            ref="dialog"
            class="workspace-dialog"
        >
            <form
                class="dialog-form"
                @submit.prevent="save"
            >
                <div class="dialog-heading">
                    <h2>{{ project ? 'New environment' : 'New project' }}</h2>
                    <button
                        class="chrome-button"
                        type="button"
                        aria-label="Close"
                        @click="dialog?.close()"
                    >
                        ✕
                    </button>
                </div>
                <label class="form-field"
                    >Name<input
                        v-model="name"
                        class="coolify-input"
                        required
                        autofocus /></label
                ><label class="form-field"
                    >Description<input
                        v-model="description"
                        class="coolify-input"
                /></label>
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
                    :disabled="saving"
                >
                    Create
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
                    >Confirm by entering {{ title
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
                        :disabled="saving || confirmation !== title"
                    >
                        Permanently Delete
                    </button>
                </div>
            </form>
        </dialog>
    </section>
</template>
