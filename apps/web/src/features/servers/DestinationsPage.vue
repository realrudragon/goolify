<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import AppIcon from '../../components/AppIcon.vue';
import PageHeading from '../../components/PageHeading.vue';
import SettingsSection from '../../components/SettingsSection.vue';
import { apiRequest } from '../../lib/api';
import { session } from '../../lib/session';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const props = defineProps<{ destinationUuid?: string; serverUuid?: string; subpage?: string }>();
interface Destination {
    id: number;
    uuid: string;
    name: string;
    network: string;
    type?: string;
    server_uuid: string;
    server_id: number;
}
interface Server {
    id: number;
    uuid: string;
    name: string;
    ip: string;
    team_id: number;
}
interface Resource {
    uuid: string;
    name: string;
    status: string;
    project_uuid: string;
    environment_uuid: string;
    type: string;
}
const lifecycle = usePageLifecycle();
const destinations = ref<Destination[]>([]);
const servers = ref<Server[]>([]);
const resources = ref<Resource[]>([]);
const resourcesLoaded = ref(false);
const search = ref('');
const mode = ref<'table' | 'grid'>('table');
const loading = ref(true);
const error = ref('');
const message = ref('');
const saving = ref(false);
const dialog = ref<HTMLDialogElement>();
const deleteDialog = ref<HTMLDialogElement>();
const name = ref('');
const network = ref('coolify');
const selectedServer = ref(props.serverUuid ?? '');
const confirmation = ref('');
const destination = computed(() =>
    destinations.value.find((d) => d.uuid === props.destinationUuid),
);
const owner = computed(() => servers.value.find((s) => s.uuid === destination.value?.server_uuid));
const role = computed(() =>
    session.me?.teams
        .find((t) => t.id === (owner.value?.team_id ?? session.me?.session_team_id))
        ?.role?.toLowerCase(),
);
const canEdit = computed(() => ['admin', 'owner'].includes(role.value ?? ''));
const filtered = computed(() =>
    destinations.value.filter(
        (d) =>
            (!props.serverUuid || props.serverUuid === d.server_uuid) &&
            [d.name, serverName(d), d.type === 'swarm' ? 'Docker Swarm' : 'Standalone Docker'].some(
                (v) => v.toLowerCase().includes(search.value.trim().toLowerCase()),
            ),
    ),
);
function serverName(d: Destination) {
    return servers.value.find((s) => s.uuid === d.server_uuid)?.name ?? d.server_uuid;
}
async function load() {
    error.value = '';
    try {
        [destinations.value, servers.value] = await Promise.all([
            apiRequest<Destination[]>('/api/v1/destinations', { signal: lifecycle.signal }),
            apiRequest<Server[]>('/api/v1/servers', { signal: lifecycle.signal }),
        ]);
        if (props.destinationUuid) {
            if (!destination.value) throw new Error('Destination not found or access denied.');
            name.value = destination.value.name;
            if (props.subpage === 'resources' || props.subpage === 'danger') {
                resources.value = await apiRequest<Resource[]>(
                    `/api/v1/destinations/${encodeURIComponent(props.destinationUuid)}/resources`,
                    { signal: lifecycle.signal },
                );
                resourcesLoaded.value = true;
            }
        }
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        loading.value = false;
    }
}
async function save() {
    if (!canEdit.value || saving.value) return;
    error.value = '';
    saving.value = true;
    try {
        if (destination.value) {
            await apiRequest(`/api/v1/destinations/${encodeURIComponent(destination.value.uuid)}`, {
                method: 'PATCH',
                body: JSON.stringify({ name: name.value }),
            });
            message.value = 'Destination updated.';
        } else {
            const created = await apiRequest<Destination>(
                `/api/v1/servers/${encodeURIComponent(selectedServer.value)}/destinations`,
                {
                    method: 'POST',
                    body: JSON.stringify({ name: name.value, network: network.value }),
                },
            );
            window.location.assign(`/destination/${created.uuid}`);
            return;
        }
        dialog.value?.close();
        await load();
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        saving.value = false;
    }
}
async function remove() {
    if (
        !destination.value ||
        confirmation.value !== destination.value.name ||
        !canEdit.value ||
        !resourcesLoaded.value ||
        resources.value.length
    )
        return;
    saving.value = true;
    try {
        await apiRequest(`/api/v1/destinations/${encodeURIComponent(destination.value.uuid)}`, {
            method: 'DELETE',
        });
        window.location.assign('/destinations');
    } catch (err) {
        error.value = (err as Error).message;
        saving.value = false;
    }
}
function openCreate() {
    name.value = '';
    selectedServer.value = props.serverUuid ?? servers.value[0]?.uuid ?? '';
    dialog.value?.showModal();
}
function setMode(value: 'table' | 'grid') {
    mode.value = value;
    localStorage.setItem('coolify-destinations-view', value);
}
function resourceHref(r: Resource) {
    return `/project/${r.project_uuid}/environment/${r.environment_uuid}/${r.type}/${r.uuid}`;
}
onMounted(() => {
    mode.value = localStorage.getItem('coolify-destinations-view') === 'grid' ? 'grid' : 'table';
    void load();
});
lifecycle.onTeamSwitch(() => {
    destinations.value = [];
    servers.value = [];
    void load();
});
</script>

<template>
    <section
        class="original-page"
        data-testid="destinations-page"
    >
        <PageHeading
            :title="destination?.name ?? 'Destinations'"
            :description="
                destination
                    ? `Docker network on ${serverName(destination)}`
                    : `${filtered.length} network endpoint${filtered.length === 1 ? '' : 's'}`
            "
        >
            <button
                v-if="!destinationUuid && canEdit && servers.length"
                type="button"
                class="coolify-btn coolify-btn-primary"
                @click="openCreate"
            >
                <AppIcon
                    name="plus"
                    :size="14"
                />New destination
            </button>
        </PageHeading>
        <p
            v-if="error"
            class="page-error"
            role="alert"
        >
            {{ error }}
        </p>
        <p
            v-if="message"
            role="status"
        >
            {{ message }}
        </p>
        <p
            v-if="loading"
            role="status"
        >
            Loading destinations…
        </p>
        <div
            v-if="destination"
            class="settings-workspace"
        >
            <nav
                class="settings-sidebar"
                aria-label="Destination settings"
            >
                <a
                    :href="`/destination/${destination.uuid}`"
                    :aria-current="subpage === 'general' ? 'page' : undefined"
                    ><AppIcon name="settings" />General</a
                ><a
                    :href="`/destination/${destination.uuid}/resources`"
                    :aria-current="subpage === 'resources' ? 'page' : undefined"
                    ><AppIcon name="projects" />Resources</a
                ><a
                    :href="`/destination/${destination.uuid}/danger`"
                    :aria-current="subpage === 'danger' ? 'page' : undefined"
                    ><AppIcon name="shield-alert" />Danger Zone</a
                >
            </nav>
            <div class="settings-content">
                <SettingsSection
                    v-if="subpage === 'resources'"
                    title="Resources"
                    ><div
                        v-if="resources.length"
                        class="original-card-grid"
                    >
                        <a
                            v-for="resource in resources"
                            :key="resource.uuid"
                            :href="resourceHref(resource)"
                            class="original-resource-card"
                            ><strong>{{ resource.name }}</strong>
                            <p>{{ resource.status }}</p></a
                        >
                    </div>
                    <p
                        v-else
                        class="page-muted"
                    >
                        No resources found
                    </p></SettingsSection
                >
                <SettingsSection
                    v-else-if="subpage === 'danger'"
                    title="Danger zone"
                    helper="Destructive actions for this destination cannot be undone."
                    ><div class="danger-row">
                        <div>
                            <strong>Delete destination</strong>
                            <p>
                                Permanently delete {{ destination.name }} from Goolify. Delete or
                                move every attached resource before deleting this destination.
                            </p>
                        </div>
                        <button
                            type="button"
                            class="coolify-btn danger-button"
                            :disabled="
                                destination.id === 0 ||
                                destination.network === 'coolify' ||
                                !canEdit ||
                                !resourcesLoaded ||
                                resources.length > 0
                            "
                            @click="deleteDialog?.showModal()"
                        >
                            Delete destination
                        </button>
                    </div></SettingsSection
                >
                <form
                    v-else
                    @submit.prevent="save"
                >
                    <SettingsSection
                        title="General"
                        helper="Docker network used to connect deployed resources."
                        ><template #actions
                            ><button
                                v-if="canEdit && name !== destination.name"
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
                                >Server IP<input
                                    :value="owner?.ip"
                                    class="coolify-input"
                                    readonly /></label
                            ><label class="form-field full-span"
                                >Docker network<input
                                    :value="destination.network"
                                    class="coolify-input"
                                    readonly
                            /></label></div
                    ></SettingsSection>
                </form>
            </div>
        </div>
        <template v-else-if="!destinationUuid && !loading && destinations.length">
            <div class="list-controls">
                <div class="list-search">
                    <AppIcon
                        name="search"
                        :size="14"
                    /><input
                        v-model="search"
                        type="search"
                        placeholder="Search destinations"
                        aria-label="Search destinations"
                    />
                </div>
                <div class="view-toggle">
                    <button
                        type="button"
                        aria-label="Table view"
                        :aria-pressed="mode === 'table'"
                        @click="setMode('table')"
                    >
                        <AppIcon name="layers" /></button
                    ><button
                        type="button"
                        aria-label="Grid view"
                        :aria-pressed="mode === 'grid'"
                        @click="setMode('grid')"
                    >
                        <AppIcon name="projects" />
                    </button>
                </div>
            </div>
            <div
                v-if="mode === 'table'"
                class="original-table-scroll"
            >
                <table class="original-table">
                    <thead>
                        <tr>
                            <th>Destination</th>
                            <th>Server</th>
                            <th>Type</th>
                        </tr>
                    </thead>
                    <tbody>
                        <tr
                            v-for="d in filtered"
                            :key="d.uuid"
                        >
                            <td>
                                <a :href="`/destination/${d.uuid}`"
                                    ><strong>{{ d.name }}</strong></a
                                >
                            </td>
                            <td>{{ serverName(d) }}</td>
                            <td>
                                <span
                                    class="status-badge"
                                    :class="{ warning: d.type === 'swarm' }"
                                    >{{
                                        d.type === 'swarm' ? 'Docker Swarm' : 'Standalone Docker'
                                    }}</span
                                >
                            </td>
                        </tr>
                    </tbody>
                </table>
            </div>
            <div
                v-else
                class="original-card-grid"
            >
                <a
                    v-for="d in filtered"
                    :key="d.uuid"
                    :href="`/destination/${d.uuid}`"
                    class="original-resource-card"
                    ><div class="resource-name">
                        <span class="resource-glyph"><AppIcon name="destinations" /></span>
                        <div>
                            <strong>{{ d.name }}</strong
                            ><small>{{ serverName(d) }}</small>
                        </div>
                    </div>
                    <span class="status-badge">{{
                        d.type === 'swarm' ? 'Docker Swarm' : 'Standalone Docker'
                    }}</span></a
                >
            </div>
            <p
                v-if="!filtered.length"
                class="original-empty"
            >
                No destinations match your search
            </p>
        </template>
        <div
            v-else-if="!loading && !error && !destinationUuid"
            class="original-empty"
        >
            <AppIcon
                name="destinations"
                :size="32"
            />
            <h2>No destinations yet</h2>
            <p>Add a destination to connect deployed resources.</p>
        </div>
        <dialog
            ref="dialog"
            class="workspace-dialog"
            @click="
                (event) => {
                    if (event.target === dialog) dialog?.close();
                }
            "
        >
            <form
                class="dialog-form"
                @submit.prevent="save"
            >
                <div class="dialog-heading">
                    <h2>New Destination</h2>
                    <button
                        type="button"
                        class="chrome-button"
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
                        required /></label
                ><label class="form-field"
                    >Docker network<input
                        v-model="network"
                        class="coolify-input"
                        required /></label
                ><label class="form-field"
                    >Server<select
                        v-model="selectedServer"
                        class="coolify-input"
                        required
                    >
                        <option
                            v-for="server in servers"
                            :key="server.uuid"
                            :value="server.uuid"
                        >
                            {{ server.name }}
                        </option>
                    </select></label
                >
                <p
                    v-if="error"
                    role="alert"
                    class="page-error"
                >
                    {{ error }}
                </p>
                <button
                    class="coolify-btn coolify-btn-primary"
                    type="submit"
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
                <h2>Confirm Destination Deletion?</h2>
                <p>This permanently deletes the destination. This action cannot be undone.</p>
                <label class="form-field"
                    >Destination Name<input
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
                        class="coolify-btn"
                        type="button"
                        @click="deleteDialog?.close()"
                    >
                        Cancel</button
                    ><button
                        class="coolify-btn danger-button"
                        :disabled="confirmation !== destination?.name || saving"
                    >
                        Permanently Delete
                    </button>
                </div>
            </form>
        </dialog>
    </section>
</template>
