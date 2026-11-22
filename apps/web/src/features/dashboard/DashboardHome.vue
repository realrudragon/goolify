<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import AppIcon from '../../components/AppIcon.vue';
import CanvasGraph from '../canvas/CanvasGraph.vue';
import { apiRequest } from '../../lib/api';
import { session } from '../../lib/session';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

interface Project {
    id: number;
    uuid: string;
    name: string;
    description?: string;
    environmentCount?: number;
    resourceCount?: number;
}
interface Server {
    uuid: string;
    name: string;
    description?: string;
    is_reachable: boolean;
    is_usable: boolean;
    force_disabled?: boolean;
    transferred_at?: string;
}
interface Resource {
    uuid: string;
    name: string;
    project_uuid?: string;
    environment_uuid?: string;
    environment_id?: number;
    git_branch?: string;
}
interface Environment {
    id: number;
    uuid: string;
    name: string;
    project_id: number;
}
interface Deployment {
    deployment_uuid: string;
    application_uuid: string;
    status: string;
    created_at: string;
}
const lifecycle = usePageLifecycle();
const projects = ref<Project[]>([]);
const servers = ref<Server[]>([]);
const apps = ref<Resource[]>([]);
const environments = ref<Environment[]>([]);
const deployments = ref<Deployment[]>([]);
const loading = ref(true);
const errors = ref<Record<string, string>>({});
const usage = ref<Record<string, { cpu_percent: number; memory_percent: number }>>({});
const projectDialog = ref<HTMLDialogElement>();
const projectName = ref('');
const projectDescription = ref('');
const editingProject = ref<Project | null>(null);
const createError = ref('');
const saving = ref(false);
const view = ref<'table' | 'grid' | 'canvas'>('table');
const serverView = ref<'table' | 'grid'>('table');
const sortAscending = ref(true);
const serverSortAscending = ref(true);
let generation = 0;
let controller: AbortController | undefined;
let refreshTimer: ReturnType<typeof setTimeout> | undefined;
const canCreate = computed(() =>
    ['admin', 'owner'].includes(
        session.me?.teams
            .find((team) => team.id === session.me?.session_team_id)
            ?.role?.toLowerCase() ?? '',
    ),
);
const sortedProjects = computed(() =>
    [...projects.value].sort(
        (a, b) => a.name.localeCompare(b.name) * (sortAscending.value ? 1 : -1),
    ),
);
const sortedServers = computed(() =>
    [...servers.value].sort(
        (a, b) => a.name.localeCompare(b.name) * (serverSortAscending.value ? 1 : -1),
    ),
);

async function load() {
    const current = ++generation;
    controller?.abort();
    controller = new AbortController();
    const signal = controller.signal;
    loading.value = true;
    errors.value = {};
    const result = await Promise.allSettled([
        apiRequest<Project[]>('/api/v1/projects', { signal }),
        apiRequest<Server[]>('/api/v1/servers', { signal }),
        apiRequest<Resource[]>('/api/v1/applications', { signal }),
        apiRequest<Resource[]>('/api/v1/services', { signal }),
        apiRequest<Resource[]>('/api/v1/databases', { signal }),
        apiRequest<Deployment[]>('/api/v1/deployments', { signal }),
    ] as const);
    if (current !== generation || signal.aborted) return;
    const [
        projectResult,
        serverResult,
        appResult,
        serviceResult,
        databaseResult,
        deploymentResult,
    ] = result;
    if (projectResult.status === 'fulfilled') projects.value = projectResult.value;
    else errors.value.projects = projectResult.reason.message;
    if (serverResult.status === 'fulfilled') servers.value = serverResult.value;
    else errors.value.servers = serverResult.reason.message;
    if (appResult.status === 'fulfilled') apps.value = appResult.value;
    if (deploymentResult.status === 'fulfilled')
        deployments.value = deploymentResult.value.slice(0, 5);
    else errors.value.deployments = deploymentResult.reason.message;
    const resourceResults = [appResult, serviceResult, databaseResult];
    const resourcesAvailable = resourceResults.every((item) => item.status === 'fulfilled');
    const resources = resourceResults.flatMap((item) =>
        item.status === 'fulfilled' ? item.value : [],
    );
    if (serverResult.status === 'fulfilled') {
        const metrics = await Promise.allSettled(
            serverResult.value.map(async (server) => {
                const values = await apiRequest<{
                    cpu_percent: number;
                    memory_percent: number;
                    disk_percent?: number;
                    has_metrics?: boolean;
                }>(`/canvas/api/servers/${encodeURIComponent(server.uuid)}/canvas/usage`, {
                    signal,
                });
                if (
                    values.has_metrics === false ||
                    !Number.isFinite(values.cpu_percent) ||
                    !Number.isFinite(values.memory_percent)
                ) {
                    return null;
                }
                return { uuid: server.uuid, values };
            }),
        );
        if (current !== generation || signal.aborted) return;
        usage.value = Object.fromEntries(
            metrics.flatMap((metric) =>
                metric.status === 'fulfilled' && metric.value
                    ? [[metric.value.uuid, metric.value.values]]
                    : [],
            ),
        );
    }
    if (projectResult.status === 'fulfilled') {
        const details = await Promise.allSettled(
            projectResult.value.map((project) =>
                apiRequest<Environment[]>(
                    `/api/v1/projects/${encodeURIComponent(project.uuid)}/environments`,
                    { signal },
                ),
            ),
        );
        if (current !== generation || signal.aborted) return;
        environments.value = details.flatMap((item) =>
            item.status === 'fulfilled' ? item.value : [],
        );
        projects.value = projectResult.value.map((project, index) => {
            const detail = details[index];
            return {
                ...project,
                environmentCount: detail.status === 'fulfilled' ? detail.value.length : undefined,
                resourceCount: resourcesAvailable
                    ? resources.filter(
                          (resource) =>
                              resource.project_uuid === project.uuid ||
                              environments.value.some(
                                  (env) =>
                                      env.project_id === project.id &&
                                      env.id === resource.environment_id,
                              ),
                      ).length
                    : undefined,
            };
        });
    }
    loading.value = false;
}

function openProject(project?: Project) {
    editingProject.value = project ?? null;
    projectName.value = project?.name ?? '';
    projectDescription.value = project?.description ?? '';
    createError.value = '';
    projectDialog.value?.showModal();
}
async function createProject() {
    if (saving.value || !projectName.value.trim()) return;
    saving.value = true;
    createError.value = '';
    try {
        await apiRequest(
            editingProject.value
                ? `/api/v1/projects/${encodeURIComponent(editingProject.value.uuid)}`
                : '/api/v1/projects',
            {
                method: editingProject.value ? 'PATCH' : 'POST',
                body: JSON.stringify({
                    name: projectName.value.trim(),
                    description: projectDescription.value.trim(),
                }),
            },
        );
        projectDialog.value?.close();
        projectName.value = '';
        projectDescription.value = '';
        await load();
    } catch (err) {
        createError.value = (err as Error).message;
    } finally {
        saving.value = false;
    }
}
function deploymentInfo(item: Deployment) {
    const app = apps.value.find((app) => app.uuid === item.application_uuid);
    const env = environments.value.find(
        (env) => env.uuid === app?.environment_uuid || env.id === app?.environment_id,
    );
    const project = projects.value.find(
        (project) => project.uuid === app?.project_uuid || project.id === env?.project_id,
    );
    return {
        title: app
            ? `${app.name}${app.git_branch ? ` (currently showing ${app.git_branch})` : ''}`
            : item.application_uuid,
        href:
            project && env
                ? `/project/${encodeURIComponent(project.uuid)}/environment/${encodeURIComponent(env.uuid)}/application/${encodeURIComponent(item.application_uuid)}/deployment/${encodeURIComponent(item.deployment_uuid)}`
                : undefined,
        context: [project?.name, env?.name].filter(Boolean).join(' / '),
    };
}
function statusLabel(status: string) {
    return (
        (
            {
                finished: 'Success',
                succeeded: 'Success',
                success: 'Success',
                failed: 'Failed',
                running: 'In progress',
                in_progress: 'In progress',
                queued: 'Queued',
                cancelled: 'Cancelled',
                canceled: 'Cancelled',
            } as Record<string, string>
        )[status] ?? status
    );
}
function age(value: string) {
    const elapsed = Math.max(0, (Date.now() - new Date(value).getTime()) / 1000);
    if (!Number.isFinite(elapsed)) return '';
    const [unit, amount] =
        elapsed >= 604800
            ? ['week', elapsed / 604800]
            : elapsed >= 86400
              ? ['day', elapsed / 86400]
              : elapsed >= 3600
                ? ['hour', elapsed / 3600]
                : elapsed >= 60
                  ? ['minute', elapsed / 60]
                  : ['second', elapsed];
    return new Intl.RelativeTimeFormat('en', { numeric: 'auto' }).format(
        -Math.floor(Number(amount)),
        unit as Intl.RelativeTimeFormatUnit,
    );
}
function resetTeam() {
    projects.value = [];
    servers.value = [];
    apps.value = [];
    environments.value = [];
    deployments.value = [];
    usage.value = {};
    projectDialog.value?.close();
    editingProject.value = null;
    projectName.value = '';
    projectDescription.value = '';
    createError.value = '';
    void load();
}
lifecycle.onTeamSwitch(resetTeam);
lifecycle.registerCleanup(() => {
    generation++;
    controller?.abort();
    clearTimeout(refreshTimer);
});
onMounted(() => {
    void load();
});
defineExpose({
    refresh: () => {
        clearTimeout(refreshTimer);
        refreshTimer = setTimeout(() => {
            void load();
        }, 200);
    },
});
</script>

<template>
    <div
        class="dashboard-layout"
        data-testid="dashboard-home"
    >
        <div class="dashboard-primary">
            <section aria-labelledby="projects-heading">
                <div class="section-heading">
                    <div>
                        <h1 id="projects-heading">Projects</h1>
                        <button
                            v-if="canCreate"
                            type="button"
                            class="section-add"
                            aria-label="Add project"
                            data-testid="new-project"
                            @click="openProject()"
                        >
                            <AppIcon name="plus" />
                        </button>
                    </div>
                    <div class="view-switcher">
                        <button
                            type="button"
                            :aria-label="
                                view === 'grid' ? 'Show project table' : 'Show project grid'
                            "
                            @click="view = view === 'grid' ? 'table' : 'grid'"
                        >
                            <AppIcon name="grid" /></button
                        ><button
                            type="button"
                            :aria-pressed="view === 'canvas'"
                            @click="view = view === 'canvas' ? 'table' : 'canvas'"
                        >
                            <AppIcon name="deploy-canvas" /> Canvas <span>Try it</span>
                        </button>
                    </div>
                </div>
                <p
                    v-if="errors.projects"
                    role="alert"
                    class="workspace-error"
                >
                    {{ errors.projects }}
                    <button
                        type="button"
                        @click="load"
                    >
                        Retry
                    </button>
                </p>
                <CanvasGraph v-if="view === 'canvas'" />
                <div
                    v-else-if="view === 'grid'"
                    class="resource-grid"
                >
                    <a
                        v-for="project in sortedProjects"
                        :key="project.uuid"
                        :href="`/project/${project.uuid}`"
                        class="resource-grid-item"
                        ><span class="resource-glyph"
                            ><AppIcon
                                name="projects"
                                :size="20" /></span
                        ><strong>{{ project.name }}</strong
                        ><span>{{ project.description || '-' }}</span></a
                    >
                    <p v-if="!projects.length">
                        {{ loading ? 'Loading projects...' : 'No projects yet' }}
                    </p>
                </div>
                <div
                    v-else
                    class="dashboard-table-frame"
                >
                    <table
                        aria-label="Projects"
                        class="dashboard-table projects-table"
                    >
                        <thead>
                            <tr>
                                <th :aria-sort="sortAscending ? 'ascending' : 'descending'">
                                    <button
                                        type="button"
                                        @click="sortAscending = !sortAscending"
                                    >
                                        Project
                                        <span aria-hidden="true">{{
                                            sortAscending ? '⌃' : '⌄'
                                        }}</span>
                                    </button>
                                </th>
                                <th>Contents</th>
                                <th>Description</th>
                                <th class="row-actions">Actions</th>
                            </tr>
                        </thead>
                        <tbody>
                            <tr
                                v-for="project in sortedProjects"
                                :key="project.uuid"
                            >
                                <td>
                                    <a
                                        :href="`/project/${project.uuid}`"
                                        class="resource-name"
                                        ><span class="resource-glyph"
                                            ><AppIcon
                                                name="projects"
                                                :size="20" /></span
                                        ><strong>{{ project.name }}</strong></a
                                    >
                                </td>
                                <td>
                                    <div class="resource-counts">
                                        <span title="Environments"
                                            ><AppIcon name="layers" />{{
                                                project.environmentCount ?? '-'
                                            }}</span
                                        ><span title="Resources"
                                            ><AppIcon name="projects" />{{
                                                project.resourceCount ?? '-'
                                            }}</span
                                        >
                                    </div>
                                </td>
                                <td class="muted">{{ project.description || '-' }}</td>
                                <td>
                                    <div class="row-actions">
                                        <a
                                            :href="`/project/${project.uuid}`"
                                            class="icon-action"
                                            :aria-label="`Add a resource to ${project.name}`"
                                            ><AppIcon name="plus" /></a
                                        ><button
                                            v-if="canCreate"
                                            type="button"
                                            class="icon-action"
                                            :aria-label="`Configure ${project.name}`"
                                            @click="openProject(project)"
                                        >
                                            <AppIcon name="settings" />
                                        </button>
                                    </div>
                                </td>
                            </tr>
                            <tr v-if="!projects.length">
                                <td
                                    colspan="4"
                                    class="table-empty"
                                >
                                    {{
                                        loading
                                            ? 'Loading projects...'
                                            : errors.projects
                                              ? 'Projects could not be loaded'
                                              : 'No projects yet'
                                    }}
                                </td>
                            </tr>
                        </tbody>
                    </table>
                </div>
            </section>
            <section aria-labelledby="servers-heading">
                <div class="section-heading">
                    <div>
                        <h2 id="servers-heading">Servers</h2>
                        <a
                            v-if="canCreate"
                            href="/servers/new"
                            class="section-add"
                            aria-label="Add server"
                            data-testid="new-server"
                            ><AppIcon name="plus"
                        /></a>
                    </div>
                    <button
                        type="button"
                        class="view-toggle"
                        :aria-label="
                            serverView === 'grid' ? 'Show server table' : 'Show server grid'
                        "
                        @click="serverView = serverView === 'grid' ? 'table' : 'grid'"
                    >
                        <AppIcon name="grid" />
                    </button>
                </div>
                <p
                    v-if="errors.servers"
                    role="alert"
                    class="workspace-error"
                >
                    {{ errors.servers }}
                    <button
                        type="button"
                        @click="load"
                    >
                        Retry
                    </button>
                </p>
                <div
                    v-if="serverView === 'grid'"
                    class="resource-grid"
                >
                    <a
                        v-for="server in sortedServers"
                        :key="server.uuid"
                        :href="`/server/${server.uuid}`"
                        class="resource-grid-item"
                        ><span class="resource-glyph"
                            ><AppIcon
                                name="servers"
                                :size="20" /></span
                        ><strong>{{ server.name }}</strong
                        ><span>{{ server.description || '-' }}</span></a
                    >
                </div>
                <div
                    v-else
                    class="dashboard-table-frame"
                >
                    <table
                        aria-label="Servers"
                        class="dashboard-table servers-table"
                    >
                        <thead>
                            <tr>
                                <th :aria-sort="serverSortAscending ? 'ascending' : 'descending'">
                                    <button
                                        type="button"
                                        @click="serverSortAscending = !serverSortAscending"
                                    >
                                        Server
                                        <span aria-hidden="true">{{
                                            serverSortAscending ? '⌃' : '⌄'
                                        }}</span>
                                    </button>
                                </th>
                                <th>Description</th>
                                <th>Usage</th>
                                <th>Status</th>
                            </tr>
                        </thead>
                        <tbody>
                            <tr
                                v-for="server in sortedServers"
                                :key="server.uuid"
                            >
                                <td>
                                    <a
                                        :href="`/server/${server.uuid}`"
                                        class="resource-name"
                                        ><span class="resource-glyph"
                                            ><AppIcon
                                                name="servers"
                                                :size="20" /></span
                                        ><strong>{{ server.name }}</strong></a
                                    >
                                </td>
                                <td class="muted">{{ server.description || '-' }}</td>
                                <td>
                                    <div
                                        class="server-usage"
                                        :title="
                                            usage[server.uuid]
                                                ? 'Latest reported usage'
                                                : 'No usage metrics available'
                                        "
                                    >
                                        <span>CPU</span
                                        ><span class="usage-track"
                                            ><i
                                                v-if="usage[server.uuid]"
                                                :style="{
                                                    width: `${Math.max(0, Math.min(100, usage[server.uuid].cpu_percent))}%`,
                                                }"
                                            ></i></span
                                        ><span>{{
                                            usage[server.uuid]
                                                ? `${Math.round(usage[server.uuid].cpu_percent)}%`
                                                : '-'
                                        }}</span
                                        ><span>Mem</span
                                        ><span class="usage-track"
                                            ><i
                                                v-if="usage[server.uuid]"
                                                :style="{
                                                    width: `${Math.max(0, Math.min(100, usage[server.uuid].memory_percent))}%`,
                                                }"
                                            ></i></span
                                        ><span>{{
                                            usage[server.uuid]
                                                ? `${Math.round(usage[server.uuid].memory_percent)}%`
                                                : '-'
                                        }}</span>
                                    </div>
                                </td>
                                <td>
                                    <span
                                        class="resource-status"
                                        :class="
                                            server.is_reachable &&
                                            server.is_usable &&
                                            !server.force_disabled &&
                                            !server.transferred_at
                                                ? 'status-success'
                                                : 'status-muted'
                                        "
                                        ><i></i
                                        >{{
                                            server.transferred_at
                                                ? 'Transferred'
                                                : server.force_disabled
                                                  ? 'Disabled'
                                                  : server.is_reachable && server.is_usable
                                                    ? 'Ready'
                                                    : 'Unreachable'
                                        }}</span
                                    >
                                </td>
                            </tr>
                            <tr v-if="!servers.length">
                                <td
                                    colspan="4"
                                    class="table-empty"
                                >
                                    {{
                                        loading
                                            ? 'Loading servers...'
                                            : errors.servers
                                              ? 'Servers could not be loaded'
                                              : 'No servers in this team.'
                                    }}
                                </td>
                            </tr>
                        </tbody>
                    </table>
                </div>
            </section>
        </div>
        <section
            class="dashboard-activity"
            aria-labelledby="deployments-heading"
        >
            <div class="section-heading"><h2 id="deployments-heading">Recent deployments</h2></div>
            <p
                v-if="errors.deployments"
                role="alert"
                class="workspace-error"
            >
                {{ errors.deployments }}
                <button
                    type="button"
                    @click="load"
                >
                    Retry
                </button>
            </p>
            <div class="recent-deployments">
                <a
                    v-for="item in deployments"
                    :key="item.deployment_uuid"
                    :href="deploymentInfo(item).href"
                    :aria-disabled="!deploymentInfo(item).href || undefined"
                    class="recent-deployment"
                    ><strong>{{ deploymentInfo(item).title }}</strong>
                    <div>
                        <span
                            class="resource-status"
                            :class="
                                ['finished', 'succeeded', 'success'].includes(item.status)
                                    ? 'status-success'
                                    : item.status === 'failed'
                                      ? 'status-error'
                                      : 'status-muted'
                            "
                            ><i></i>{{ statusLabel(item.status) }}</span
                        ><span class="deployment-context">{{ deploymentInfo(item).context }}</span
                        ><time
                            :datetime="item.created_at"
                            :title="new Date(item.created_at).toLocaleString()"
                            >{{ age(item.created_at) }}</time
                        >
                    </div></a
                >
                <p
                    v-if="!deployments.length"
                    class="table-empty"
                >
                    {{
                        loading
                            ? 'Loading deployments...'
                            : errors.deployments
                              ? 'Deployments could not be loaded'
                              : 'No recent deployments'
                    }}
                </p>
            </div>
        </section>
        <dialog
            ref="projectDialog"
            class="workspace-dialog"
            aria-labelledby="new-project-title"
        >
            <form @submit.prevent="createProject">
                <div class="dialog-heading">
                    <h2 id="new-project-title">
                        {{ editingProject ? 'Edit project' : 'New project' }}
                    </h2>
                    <button
                        type="button"
                        class="chrome-button"
                        aria-label="Close project form"
                        @click="projectDialog?.close()"
                    >
                        ✕
                    </button>
                </div>
                <p
                    v-if="createError"
                    role="alert"
                    class="workspace-error"
                >
                    {{ createError }}
                </p>
                <label for="project-name">Name</label
                ><input
                    id="project-name"
                    v-model="projectName"
                    class="coolify-input"
                    aria-label="New project name"
                    required
                    autofocus
                /><label for="project-description">Description</label
                ><input
                    id="project-description"
                    v-model="projectDescription"
                    class="coolify-input"
                    aria-label="Project description"
                />
                <div class="dialog-actions">
                    <button
                        type="button"
                        class="coolify-btn"
                        @click="projectDialog?.close()"
                    >
                        Cancel</button
                    ><button
                        type="submit"
                        class="coolify-btn coolify-btn-primary"
                        :disabled="saving"
                    >
                        {{ saving ? 'Saving...' : editingProject ? 'Save' : 'Create Project' }}
                    </button>
                </div>
            </form>
        </dialog>
    </div>
</template>
