<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import PageHeading from '../../components/PageHeading.vue';
import SettingsSection from '../../components/SettingsSection.vue';
import AppIcon from '../../components/AppIcon.vue';
import { apiRequest } from '../../lib/api';
import { loadSession, session } from '../../lib/session';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const props = defineProps<{ serverUuid?: string }>();
interface Server {
    uuid: string;
    name: string;
    team_id: number;
    transferred_at?: string;
}
interface Registry {
    uuid: string;
    registry: string;
    username: string;
    verified: boolean;
    last_checked_at?: string;
}
const lifecycle = usePageLifecycle();
const servers = ref<Server[]>([]);
const rows = ref<Record<string, Registry[]>>({});
const errors = ref<Record<string, string>>({});
const loading = ref(false);
const busy = ref(false);
const error = ref('');
const message = ref('');
const dialog = ref<HTMLDialogElement>();
const deleteDialog = ref<HTMLDialogElement>();
const pendingDelete = ref<{ server: Server; registry: Registry }>();
const selected = ref<string[]>([]);
const form = ref({ registry: '', username: '', password: '' });
let generation = 0;
const editableServers = computed(() => servers.value.filter(canEdit));
function canEdit(server: Server) {
    return (
        !server.transferred_at &&
        ['admin', 'owner'].includes(
            session.me?.teams.find((t) => t.id === server.team_id)?.role?.toLowerCase() ?? '',
        )
    );
}
function endpoint(uuid: string) {
    return `/api/v1/servers/${encodeURIComponent(uuid)}/registries`;
}
async function refresh(server: Server, version = generation) {
    try {
        const result = await apiRequest<Registry[]>(endpoint(server.uuid), {
            signal: lifecycle.signal,
        });
        if (version !== generation) return;
        rows.value[server.uuid] = result;
        delete errors.value[server.uuid];
    } catch (e) {
        if (version === generation && !lifecycle.signal.aborted)
            errors.value[server.uuid] = (e as Error).message;
    }
}
async function load() {
    const version = ++generation;
    loading.value = true;
    error.value = '';
    rows.value = {};
    errors.value = {};
    servers.value = [];
    try {
        await loadSession();
        const result = props.serverUuid
            ? [
                  await apiRequest<Server>(
                      `/api/v1/servers/${encodeURIComponent(props.serverUuid)}`,
                      { signal: lifecycle.signal },
                  ),
              ]
            : await apiRequest<Server[]>('/api/v1/servers', { signal: lifecycle.signal });
        if (version !== generation) return;
        servers.value = result;
        await Promise.all(result.map((server) => refresh(server, version)));
    } catch (e) {
        if (version === generation && !lifecycle.signal.aborted) error.value = (e as Error).message;
    } finally {
        if (version === generation) loading.value = false;
    }
}
function openLogin(server?: Server) {
    selected.value = server ? [server.uuid] : editableServers.value.map((s) => s.uuid);
    form.value = { registry: '', username: '', password: '' };
    error.value = '';
    message.value = '';
    dialog.value?.showModal();
}
async function login() {
    if (busy.value || !selected.value.length) return;
    busy.value = true;
    error.value = '';
    message.value = '';
    const version = generation;
    const failures: string[] = [];
    const failedIDs: string[] = [];
    const successes: string[] = [];
    try {
        for (const uuid of selected.value) {
            const server = editableServers.value.find((s) => s.uuid === uuid);
            if (!server) continue;
            try {
                await apiRequest(endpoint(uuid), {
                    method: 'POST',
                    body: JSON.stringify(form.value),
                    signal: lifecycle.signal,
                });
                successes.push(server.name);
                await refresh(server, version);
            } catch (e) {
                failures.push(`${server.name}: ${(e as Error).message}`);
                failedIDs.push(uuid);
            }
            if (generation !== version) return;
        }
        message.value = successes.length ? `Logged in on ${successes.join(', ')}.` : '';
        error.value = failures.join('\n');
        if (failures.length) selected.value = failedIDs;
        else {
            dialog.value?.close();
            form.value.password = '';
        }
    } finally {
        busy.value = false;
    }
}
async function testLogin(server: Server, registry: Registry) {
    if (busy.value) return;
    const version = generation;
    busy.value = true;
    error.value = '';
    message.value = '';
    try {
        await apiRequest(`${endpoint(server.uuid)}/${encodeURIComponent(registry.uuid)}/check`, {
            method: 'POST',
            signal: lifecycle.signal,
        });
        if (version === generation) message.value = `Registry login succeeded on ${server.name}.`;
    } catch (e) {
        if (version === generation) error.value = (e as Error).message;
    } finally {
        if (version === generation) await refresh(server, version);
        busy.value = false;
    }
}
async function logout() {
    const target = pendingDelete.value;
    if (!target || busy.value) return;
    const version = generation;
    busy.value = true;
    error.value = '';
    try {
        await apiRequest(
            `${endpoint(target.server.uuid)}/${encodeURIComponent(target.registry.uuid)}`,
            { method: 'DELETE', signal: lifecycle.signal },
        );
        if (version !== generation) return;
        deleteDialog.value?.close();
        message.value = `Logged out from ${target.registry.registry} on ${target.server.name}.`;
        await refresh(target.server, version);
    } catch (e) {
        if (version === generation) error.value = (e as Error).message;
    } finally {
        busy.value = false;
    }
}
lifecycle.onTeamSwitch(() => {
    generation++;
    dialog.value?.close();
    deleteDialog.value?.close();
    form.value.password = '';
    message.value = '';
    void load();
});
onMounted(load);
</script>

<template>
    <section
        data-testid="registries-page"
        class="settings-content"
    >
        <PageHeading
            v-if="!serverUuid"
            title="Docker Registries"
            description="Registry logins managed on each server. Use Multi-server login, or log in on an individual server."
        >
            <button
                v-if="editableServers.length"
                type="button"
                class="coolify-btn coolify-btn-primary"
                :disabled="busy || loading"
                @click="openLogin()"
            >
                Multi-server login
            </button>
        </PageHeading>
        <p
            v-if="error && !dialog?.open && !deleteDialog?.open"
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
        <p
            v-if="loading"
            role="status"
            class="page-muted"
        >
            Loading registries…
        </p>
        <div
            v-for="server in servers"
            :key="server.uuid"
            data-testid="registry-server-card"
        >
            <SettingsSection :title="server.name">
                <template #actions>
                    <button
                        type="button"
                        class="coolify-btn"
                        :disabled="busy || loading"
                        @click="refresh(server)"
                    >
                        <AppIcon
                            name="refresh3"
                            :size="14"
                        />Refresh
                    </button>
                    <button
                        v-if="canEdit(server)"
                        type="button"
                        class="coolify-btn"
                        :disabled="busy"
                        @click="openLogin(server)"
                    >
                        New
                    </button>
                </template>
                <p
                    v-if="errors[server.uuid]"
                    role="alert"
                    class="page-error"
                >
                    {{ errors[server.uuid] }}
                </p>
                <table
                    v-else-if="rows[server.uuid]?.length"
                    class="original-table"
                >
                    <thead>
                        <tr>
                            <th>Registry</th>
                            <th>Username</th>
                            <th>Last login</th>
                            <th>Actions</th>
                        </tr>
                    </thead>
                    <tbody>
                        <tr
                            v-for="registry in rows[server.uuid]"
                            :key="registry.uuid"
                        >
                            <td>{{ registry.registry }}</td>
                            <td>{{ registry.username }}</td>
                            <td>
                                <span
                                    :class="[
                                        'status-dot',
                                        registry.verified ? 'status-ready' : 'status-error',
                                    ]"
                                ></span
                                >{{ registry.verified ? 'Success' : 'Not verified' }}
                            </td>
                            <td>
                                <div
                                    v-if="canEdit(server)"
                                    class="page-actions"
                                >
                                    <button
                                        type="button"
                                        class="coolify-btn"
                                        :disabled="busy"
                                        @click="testLogin(server, registry)"
                                    >
                                        Test</button
                                    ><button
                                        type="button"
                                        class="coolify-btn"
                                        :disabled="busy"
                                        @click="
                                            pendingDelete = { server, registry };
                                            deleteDialog?.showModal();
                                        "
                                    >
                                        Delete
                                    </button>
                                </div>
                            </td>
                        </tr>
                    </tbody>
                </table>
                <p
                    v-else-if="!loading"
                    class="page-muted"
                >
                    No registry logins managed by Goolify on this server.
                </p>
            </SettingsSection>
        </div>
        <p
            v-if="!loading && !error && !servers.length"
            class="original-empty"
        >
            No servers yet. Add a server to manage its Docker registry logins.
        </p>
        <dialog
            ref="dialog"
            class="original-dialog"
            data-testid="registry-login-dialog"
            @close="form.password = ''"
            @cancel="busy && $event.preventDefault()"
        >
            <form @submit.prevent="login">
                <h2>
                    {{
                        serverUuid || selected.length === 1
                            ? 'Log in to a registry'
                            : 'Log in on multiple servers'
                    }}
                </h2>
                <p class="page-muted">Runs docker login on each server that you choose.</p>
                <p
                    v-if="error"
                    role="alert"
                    class="page-error"
                >
                    {{ error }}
                </p>
                <div class="settings-content">
                    <label class="form-field"
                        >Registry<input
                            v-model="form.registry"
                            class="coolify-input"
                            placeholder="ghcr.io"
                            required
                            :disabled="busy"
                    /></label>
                    <label class="form-field"
                        >Username<input
                            v-model="form.username"
                            class="coolify-input"
                            required
                            :disabled="busy"
                            autocomplete="username"
                    /></label>
                    <label class="form-field"
                        >Password or token<input
                            v-model="form.password"
                            type="password"
                            class="coolify-input"
                            required
                            :disabled="busy"
                            autocomplete="new-password"
                    /></label>
                    <fieldset v-if="!serverUuid">
                        <legend>Servers</legend>
                        <label
                            v-for="server in editableServers"
                            :key="server.uuid"
                            class="checkbox-row"
                            ><input
                                v-model="selected"
                                type="checkbox"
                                :value="server.uuid"
                                :disabled="busy"
                            />{{ server.name }}</label
                        >
                    </fieldset>
                    <div class="page-actions">
                        <button
                            type="button"
                            class="coolify-btn"
                            :disabled="busy"
                            @click="dialog?.close()"
                        >
                            Cancel</button
                        ><button
                            type="submit"
                            class="coolify-btn coolify-btn-primary"
                            :disabled="busy || !selected.length"
                        >
                            {{ busy ? 'Logging in…' : 'Log in' }}
                        </button>
                    </div>
                </div>
            </form>
        </dialog>
        <dialog
            ref="deleteDialog"
            class="original-dialog"
            @cancel="busy && $event.preventDefault()"
        >
            <form @submit.prevent="logout">
                <h2>Delete registry login</h2>
                <p>
                    Runs docker logout on {{ pendingDelete?.server.name }}. Deployments using
                    private images from {{ pendingDelete?.registry.registry }} require a new login.
                </p>
                <p
                    v-if="error"
                    role="alert"
                    class="page-error"
                >
                    {{ error }}
                </p>
                <div class="page-actions">
                    <button
                        type="button"
                        class="coolify-btn"
                        :disabled="busy"
                        @click="deleteDialog?.close()"
                    >
                        Cancel</button
                    ><button
                        type="submit"
                        class="coolify-btn coolify-btn-danger"
                        :disabled="busy"
                    >
                        {{ busy ? 'Logging out…' : 'Delete' }}
                    </button>
                </div>
            </form>
        </dialog>
    </section>
</template>
