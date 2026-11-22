<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import AppIcon from '../../components/AppIcon.vue';
import PageHeading from '../../components/PageHeading.vue';
import { apiRequest } from '../../lib/api';
import { session } from '../../lib/session';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

interface Server {
    id: number;
    uuid: string;
    name: string;
    description?: string;
    is_reachable: boolean;
    is_usable: boolean;
    force_disabled: boolean;
    transferred_at?: string;
}
const lifecycle = usePageLifecycle();
const servers = ref<Server[]>([]);
const counts = ref<Record<string, number>>({});
const search = ref('');
const mode = ref<'table' | 'grid'>('table');
const error = ref('');
const loading = ref(true);
const canCreate = computed(() =>
    ['admin', 'owner'].includes(
        session.me?.teams.find((t) => t.id === session.me?.session_team_id)?.role?.toLowerCase() ??
            '',
    ),
);
function status(server: Server) {
    if (server.transferred_at) return 'Transferred away';
    if (server.force_disabled) return 'Disabled';
    return server.is_reachable && server.is_usable ? 'Ready' : 'Validation required';
}
const filtered = computed(() =>
    servers.value.filter((server) =>
        [server.name, server.description, status(server)].some((value) =>
            value?.toLowerCase().includes(search.value.trim().toLowerCase()),
        ),
    ),
);
async function load() {
    error.value = '';
    try {
        servers.value = await apiRequest<Server[]>('/api/v1/servers', { signal: lifecycle.signal });
        await Promise.allSettled(
            servers.value.map(async (server) => {
                const resources = await apiRequest<unknown[]>(
                    `/api/v1/servers/${encodeURIComponent(server.uuid)}/resources`,
                    { signal: lifecycle.signal },
                );
                counts.value[server.uuid] = resources.length;
            }),
        );
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        loading.value = false;
    }
}
function setMode(value: 'table' | 'grid') {
    mode.value = value;
    localStorage.setItem('coolify-servers-view', value);
}
onMounted(() => {
    mode.value = localStorage.getItem('coolify-servers-view') === 'grid' ? 'grid' : 'table';
    void load();
});
lifecycle.onTeamSwitch(() => {
    servers.value = [];
    counts.value = {};
    void load();
});
</script>

<template>
    <section
        data-testid="servers-page"
        class="original-page"
    >
        <PageHeading title="Servers"
            ><a
                v-if="canCreate"
                href="/servers/new"
                class="coolify-btn coolify-btn-primary"
                ><AppIcon
                    name="plus"
                    :size="14"
                />New server</a
            ></PageHeading
        >
        <p
            v-if="error"
            class="page-error"
            role="alert"
        >
            {{ error }}
            <button
                type="button"
                class="coolify-btn"
                @click="load"
            >
                Retry
            </button>
        </p>
        <p
            v-if="loading"
            class="page-muted"
            role="status"
        >
            Loading servers…
        </p>
        <template v-else-if="servers.length">
            <div class="list-controls">
                <div class="list-search">
                    <AppIcon
                        name="search"
                        :size="14"
                    /><input
                        v-model="search"
                        type="search"
                        placeholder="Search servers"
                        aria-label="Search servers"
                    />
                </div>
                <div class="view-toggle">
                    <button
                        type="button"
                        :aria-pressed="mode === 'table'"
                        aria-label="Table view"
                        @click="setMode('table')"
                    >
                        <AppIcon
                            name="layers"
                            :size="15"
                        /></button
                    ><button
                        type="button"
                        :aria-pressed="mode === 'grid'"
                        aria-label="Grid view"
                        @click="setMode('grid')"
                    >
                        <AppIcon
                            name="projects"
                            :size="15"
                        /></button
                    ><a href="/servers/canvas">Canvas <span class="try-badge">Try it</span></a>
                </div>
            </div>
            <div
                v-if="mode === 'table'"
                class="original-table-scroll"
            >
                <table class="original-table server-index-table">
                    <thead>
                        <tr>
                            <th>Server</th>
                            <th>Resources</th>
                            <th>Status</th>
                        </tr>
                    </thead>
                    <tbody>
                        <tr
                            v-for="server in filtered"
                            :key="server.uuid"
                        >
                            <td>
                                <a
                                    :href="`/server/${server.uuid}`"
                                    class="resource-name"
                                    ><span class="resource-glyph"
                                        ><AppIcon
                                            name="servers"
                                            :size="16" /></span
                                    ><span
                                        ><strong>{{ server.name }}</strong
                                        ><small v-if="server.description">{{
                                            server.description
                                        }}</small></span
                                    ></a
                                >
                            </td>
                            <td>{{ counts[server.uuid] ?? '-' }}</td>
                            <td>
                                <span class="status-label"
                                    ><i
                                        :class="
                                            status(server) === 'Ready'
                                                ? 'status-dot success'
                                                : 'status-dot warning'
                                        "
                                    />{{ status(server) }}</span
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
                    v-for="server in filtered"
                    :key="server.uuid"
                    :href="`/server/${server.uuid}`"
                    class="original-resource-card"
                    ><span class="resource-glyph"
                        ><AppIcon
                            name="servers"
                            :size="16" /></span
                    ><strong>{{ server.name }}</strong>
                    <p>{{ server.description }}</p>
                    <span class="status-label"
                        ><i
                            :class="
                                status(server) === 'Ready'
                                    ? 'status-dot success'
                                    : 'status-dot warning'
                            "
                        />{{ status(server) }}</span
                    ></a
                >
            </div>
            <p
                v-if="!filtered.length"
                class="original-empty"
            >
                No servers match your search
            </p>
        </template>
        <div
            v-else-if="!error"
            class="original-empty"
        >
            <AppIcon
                name="servers"
                :size="32"
            />
            <h2>No servers yet</h2>
            <p>Add a server to deploy applications, databases, and services.</p>
        </div>
    </section>
</template>
