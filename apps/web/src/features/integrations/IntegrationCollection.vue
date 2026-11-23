<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import PageHeading from '../../components/PageHeading.vue';
import SettingsSection from '../../components/SettingsSection.vue';
import AppIcon from '../../components/AppIcon.vue';
import { apiRequest } from '../../lib/api';
import { session } from '../../lib/session';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const props = defineProps<{ kind: string; uuid?: string; subpage?: string }>();
type Item = Record<string, any> & {
    uuid: string;
    name: string;
    provider?: string;
    team_id?: number;
};
interface Field {
    key: string;
    label: string;
    type?: string;
    required?: boolean;
    options?: string[];
    createOnly?: boolean;
}
const configs: Record<
    string,
    {
        title: string;
        singular: string;
        icon: string;
        endpoint: string;
        route: string;
        description: string;
        fields: Field[];
    }
> = {
    'private-keys': {
        title: 'Private Keys',
        singular: 'Private Key',
        icon: 'keys',
        endpoint: '/api/v1/security/keys',
        route: '/security/private-key',
        description: 'SSH keys used to connect to your servers.',
        fields: [
            { key: 'name', label: 'Name', required: true },
            { key: 'description', label: 'Description' },
            {
                key: 'private_key',
                label: 'Private Key',
                type: 'textarea',
                required: true,
                createOnly: true,
            },
        ],
    },
    'cloud-tokens': {
        title: 'Cloud Tokens',
        singular: 'Cloud Token',
        icon: 'cloud',
        endpoint: '/api/v1/cloud-tokens',
        route: '/security/cloud-tokens',
        description: 'Manage credentials for your cloud providers.',
        fields: [
            {
                key: 'provider',
                label: 'Provider',
                options: ['hetzner', 'digitalocean', 'vultr', 'hostinger'],
                required: true,
                createOnly: true,
            },
            { key: 'name', label: 'Name', required: true },
            { key: 'description', label: 'Description' },
            { key: 'token', label: 'Token', type: 'password' },
        ],
    },
    'integration-tokens': {
        title: 'Integration Tokens',
        singular: 'Integration Token',
        icon: 'network',
        endpoint: '/api/v1/security/integration-tokens',
        route: '/security/integration-tokens',
        description: 'Credentials for DNS providers and external integrations.',
        fields: [
            {
                key: 'provider',
                label: 'Provider',
                options: ['cloudflare', 'vault', 'infisical'],
                required: true,
                createOnly: true,
            },
            { key: 'name', label: 'Name', required: true },
            { key: 'token', label: 'Token', type: 'password' },
        ],
    },
    'cloud-init-scripts': {
        title: 'Cloud-Init Scripts',
        singular: 'Cloud-Init Script',
        icon: 'file-content',
        endpoint: '/api/v1/cloud-init-scripts',
        route: '/security/cloud-init-scripts',
        description: 'Scripts to configure newly provisioned servers.',
        fields: [
            { key: 'name', label: 'Name', required: true },
            { key: 'script', label: 'Script', type: 'textarea', required: true },
        ],
    },
    storages: {
        title: 'Storage',
        singular: 'Storage',
        icon: 'storages',
        endpoint: '/api/v1/s3-storages',
        route: '/storages',
        description: 'Storage destinations for your backups.',
        fields: [
            { key: 'name', label: 'Name', required: true },
            { key: 'description', label: 'Description' },
            { key: 'endpoint', label: 'Endpoint', type: 'url', required: true },
            { key: 'bucket', label: 'Bucket', required: true },
            { key: 'region', label: 'Region', required: true },
            { key: 'key', label: 'Access Key ID', type: 'password' },
            { key: 'secret', label: 'Secret Access Key', type: 'password' },
        ],
    },
    sources: {
        title: 'Sources',
        singular: 'Source',
        icon: 'sources',
        endpoint: '',
        route: '/sources',
        description: 'Connect Git providers to deploy private repositories.',
        fields: [
            {
                key: 'provider',
                label: 'Git provider',
                options: ['github', 'gitlab', 'gitea'],
                required: true,
                createOnly: true,
            },
            { key: 'name', label: 'Name', required: true },
            { key: 'api_url', label: 'API URL', type: 'url' },
            { key: 'html_url', label: 'HTML URL', type: 'url' },
        ],
    },
};
const config = computed(() => configs[props.kind]);
const lifecycle = usePageLifecycle();
const items = ref<Item[]>([]);
const search = ref('');
const mode = ref<'table' | 'grid'>('table');
const linkedResources = ref<{ uuid: string; name: string; type: string }[]>([]);
const error = ref('');
const message = ref('');
const loading = ref(true);
const saving = ref(false);
const dialog = ref<HTMLDialogElement>();
const deleteDialog = ref<HTMLDialogElement>();
const editing = ref<Item | null>(null);
const form = ref<Record<string, any>>({});
const confirmation = ref('');
const detail = computed(() => items.value.find((item) => item.uuid === props.uuid));
const canEdit = computed(() =>
    ['admin', 'owner'].includes(
        session.me?.teams
            .find(
                (t) =>
                    t.id ===
                    (editing.value?.team_id ??
                        detail.value?.team_id ??
                        session.me?.session_team_id),
            )
            ?.role?.toLowerCase() ?? '',
    ),
);
const filtered = computed(() =>
    items.value.filter((item) =>
        [item.name, item.description, item.provider, item.endpoint].some(
            (v) =>
                typeof v === 'string' &&
                v.toLowerCase().includes(search.value.trim().toLowerCase()),
        ),
    ),
);
const visibleFields = computed(() =>
    config.value.fields.filter((f) => !f.createOnly || !editing.value),
);
function itemEndpoint(item: Item | Record<string, any>) {
    return props.kind === 'sources' ? `/api/v1/${item.provider}-apps` : config.value.endpoint;
}
function href(item: Item) {
    return props.kind === 'sources'
        ? `/source/${item.provider}/${item.uuid}`
        : `${config.value.route}/${item.uuid}`;
}
async function load() {
    error.value = '';
    try {
        items.value =
            props.kind === 'sources'
                ? (
                      await Promise.all(
                          ['github', 'gitlab', 'gitea'].map(async (provider) =>
                              (
                                  await apiRequest<Item[]>(`/api/v1/${provider}-apps`, {
                                      signal: lifecycle.signal,
                                  })
                              ).map((item) => ({ ...item, provider })),
                          ),
                      )
                  ).flat()
                : await apiRequest<Item[]>(config.value.endpoint, { signal: lifecycle.signal });
        if (props.uuid) {
            if (!detail.value)
                throw new Error(`${config.value.singular} not found or access denied.`);
            setEditing(detail.value);
            if (props.subpage === 'resources') {
                linkedResources.value = await apiRequest(
                    `/api/v1/integration-resources?kind=${encodeURIComponent(props.kind)}&uuid=${encodeURIComponent(props.uuid)}`,
                    { signal: lifecycle.signal },
                );
            }
        }
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        loading.value = false;
    }
}
function setEditing(item: Item | null) {
    editing.value = item;
    form.value = Object.fromEntries(
        config.value.fields.map((f) => [
            f.key,
            item?.[f.key] ?? f.options?.[0] ?? (f.key === 'region' ? 'us-east-1' : ''),
        ]),
    );
    // Secret fields are write-only. An untouched empty field never replaces a stored credential.
    for (const field of config.value.fields)
        if (field.type === 'password') form.value[field.key] = '';
}
function open(item: Item | null = null) {
    setEditing(item);
    error.value = '';
    dialog.value?.showModal();
}
async function save() {
    if (!canEdit.value || saving.value) return;
    error.value = '';
    saving.value = true;
    try {
        const payload = Object.fromEntries(
            visibleFields.value
                .filter((f) => !editing.value || f.type !== 'password' || form.value[f.key] !== '')
                .map((f) => [f.key, form.value[f.key]]),
        );
        if (props.kind === 'integration-tokens' && !editing.value)
            payload.capabilities = ['dns', 'secrets'];
        const url =
            itemEndpoint(editing.value ?? form.value) +
            (editing.value ? `/${encodeURIComponent(editing.value.uuid)}` : '');
        const result = await apiRequest<Item>(url, {
            method: editing.value ? 'PATCH' : 'POST',
            body: JSON.stringify(payload),
        });
        dialog.value?.close();
        message.value = `${config.value.singular} saved.`;
        if (!editing.value && props.kind === 'storages' && result.uuid)
            window.location.assign(href(result));
        else await load();
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        saving.value = false;
    }
}
function confirmDelete(item: Item) {
    editing.value = item;
    confirmation.value = '';
    deleteDialog.value?.showModal();
}
async function remove() {
    if (
        !editing.value ||
        confirmation.value !== editing.value.name ||
        !canEdit.value ||
        saving.value
    )
        return;
    saving.value = true;
    try {
        await apiRequest(
            `${itemEndpoint(editing.value)}/${encodeURIComponent(editing.value.uuid)}`,
            { method: 'DELETE' },
        );
        deleteDialog.value?.close();
        if (props.uuid) window.location.assign(config.value.route);
        else await load();
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        saving.value = false;
    }
}
onMounted(() => {
    const saved = localStorage.getItem(`coolify-${props.kind}-view`);
    if (saved === 'grid') mode.value = 'grid';
    void load();
});
async function validateStorage(storageUuid: string) {
    if (!canEdit.value || saving.value) return;
    saving.value = true;
    error.value = '';
    message.value = '';
    try {
        const res = await apiRequest<{ message?: string }>(
            `/api/v1/s3-storages/${encodeURIComponent(storageUuid)}/validate`,
            { method: 'POST', body: '{}' },
        );
        message.value = res.message ?? 'S3 storage validated.';
        await load();
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        saving.value = false;
    }
}
function setMode(value: 'table' | 'grid') {
    mode.value = value;
    localStorage.setItem(`coolify-${props.kind}-view`, value);
}
</script>

<template>
    <section
        class="original-page"
        :data-testid="`${kind}-page`"
    >
        <PageHeading
            :title="detail?.name ?? config.title"
            :description="config.description"
            ><button
                v-if="canEdit && !uuid"
                class="coolify-btn coolify-btn-primary"
                type="button"
                @click="open()"
            >
                <AppIcon
                    name="plus"
                    :size="14"
                />New {{ config.singular.toLowerCase() }}
            </button></PageHeading
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
            Loading {{ config.title.toLowerCase() }}…
        </p>
        <div
            v-if="detail"
            class="settings-workspace"
        >
            <nav
                class="settings-sidebar"
                :aria-label="`${config.singular} settings`"
            >
                <a
                    :href="href(detail)"
                    :aria-current="subpage === 'general' || !subpage ? 'page' : undefined"
                    ><AppIcon name="settings" />General</a
                ><a
                    :href="`${href(detail)}/resources`"
                    :aria-current="subpage === 'resources' ? 'page' : undefined"
                    ><AppIcon name="projects" />Resources</a
                ><a
                    :href="`${href(detail)}/danger`"
                    :aria-current="subpage === 'danger' ? 'page' : undefined"
                    ><AppIcon name="shield-alert" />Danger Zone</a
                >
            </nav>
            <div class="settings-content">
                <SettingsSection
                    v-if="subpage === 'danger'"
                    title="Danger zone"
                    ><div class="danger-row">
                        <div>
                            <strong>Delete {{ config.singular.toLowerCase() }}</strong>
                            <p>This action cannot be undone.</p>
                        </div>
                        <button
                            type="button"
                            class="coolify-btn danger-button"
                            :disabled="!canEdit"
                            @click="confirmDelete(detail)"
                        >
                            Delete
                        </button>
                    </div></SettingsSection
                >
                <SettingsSection
                    v-else-if="subpage === 'resources'"
                    title="Resources"
                    ><div
                        v-if="linkedResources.length"
                        class="original-table-scroll"
                    >
                        <table class="original-table">
                            <thead>
                                <tr>
                                    <th>Name</th>
                                    <th>Type</th>
                                    <th>UUID</th>
                                </tr>
                            </thead>
                            <tbody>
                                <tr
                                    v-for="res in linkedResources"
                                    :key="res.uuid"
                                >
                                    <td>{{ res.name }}</td>
                                    <td>{{ res.type }}</td>
                                    <td class="code-text">{{ res.uuid }}</td>
                                </tr>
                            </tbody>
                        </table>
                    </div>
                    <p
                        v-else
                        class="page-muted"
                    >
                        No resources are currently using this {{ config.singular.toLowerCase() }}.
                    </p></SettingsSection
                >
                <form
                    v-else
                    @submit.prevent="save"
                >
                    <SettingsSection title="General"
                        ><template #actions
                            ><button
                                v-if="canEdit && kind === 'storages' && detail"
                                class="coolify-btn"
                                type="button"
                                :disabled="saving"
                                @click="validateStorage(detail.uuid)"
                            >
                                Validate Connection</button
                            ><button
                                v-if="canEdit"
                                class="coolify-btn coolify-btn-primary"
                                type="submit"
                                :disabled="saving"
                            >
                                Save
                            </button></template
                        >
                        <div class="form-grid">
                            <label
                                v-for="field in visibleFields"
                                :key="field.key"
                                class="form-field"
                                :class="{ 'full-span': field.type === 'textarea' }"
                                >{{ field.label
                                }}<textarea
                                    v-if="field.type === 'textarea'"
                                    v-model="form[field.key]"
                                    class="coolify-input"
                                    rows="10"
                                    :required="field.required"
                                    :readonly="!canEdit" /><input
                                    v-else
                                    v-model="form[field.key]"
                                    class="coolify-input"
                                    :type="field.type ?? 'text'"
                                    :required="field.required"
                                    :readonly="!canEdit"
                                    :placeholder="
                                        field.type === 'password'
                                            ? 'Leave empty to keep the current value'
                                            : undefined
                                    "
                            /></label></div
                    ></SettingsSection>
                </form>
            </div>
        </div>
        <template v-else-if="!uuid && items.length">
            <div class="list-controls">
                <div class="list-search">
                    <AppIcon
                        name="search"
                        :size="14"
                    /><input
                        v-model="search"
                        type="search"
                        :placeholder="`Search ${config.title.toLowerCase()}`"
                        :aria-label="`Search ${config.title.toLowerCase()}`"
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
                        <AppIcon name="grid" />
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
                            <th>{{ config.singular }}</th>
                            <th>
                                {{
                                    kind === 'storages'
                                        ? 'Endpoint'
                                        : kind === 'private-keys'
                                          ? 'Description'
                                          : 'Provider'
                                }}
                            </th>
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
                                    :href="href(item)"
                                    class="resource-name"
                                    ><span class="resource-glyph"
                                        ><AppIcon :name="config.icon" /></span
                                    ><strong>{{ item.name || item.uuid }}</strong></a
                                >
                            </td>
                            <td>{{ item.endpoint || item.provider || item.description || '-' }}</td>
                            <td>
                                <div
                                    v-if="canEdit"
                                    class="page-actions"
                                >
                                    <button
                                        type="button"
                                        class="chrome-button"
                                        :aria-label="`Edit ${item.name}`"
                                        @click="open(item)"
                                    >
                                        <AppIcon name="settings" /></button
                                    ><button
                                        type="button"
                                        class="chrome-button"
                                        :aria-label="`Delete ${item.name}`"
                                        @click="confirmDelete(item)"
                                    >
                                        ✕
                                    </button>
                                </div>
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
                    v-for="item in filtered"
                    :key="item.uuid"
                    :href="href(item)"
                    class="original-resource-card"
                    ><span class="resource-glyph"><AppIcon :name="config.icon" /></span
                    ><strong>{{ item.name || item.uuid }}</strong>
                    <p>{{ item.endpoint || item.provider || item.description }}</p></a
                >
            </div>
            <p
                v-if="!filtered.length"
                class="original-empty"
            >
                No {{ config.title.toLowerCase() }} match your search
            </p>
        </template>
        <div
            v-else-if="!uuid && !loading && !error"
            class="original-empty"
        >
            <AppIcon
                :name="config.icon"
                :size="32"
            />
            <h2>No {{ config.title.toLowerCase() }} yet</h2>
            <p>{{ config.description }}</p>
        </div>
        <dialog
            ref="dialog"
            class="workspace-dialog"
        >
            <form
                class="dialog-form"
                @submit.prevent="save"
            >
                <div class="dialog-heading">
                    <h2>{{ editing ? 'Edit' : 'New' }} {{ config.singular }}</h2>
                    <button
                        type="button"
                        class="chrome-button"
                        aria-label="Close"
                        @click="dialog?.close()"
                    >
                        ✕
                    </button>
                </div>
                <label
                    v-for="field in visibleFields"
                    :key="field.key"
                    class="form-field"
                    >{{ field.label
                    }}<select
                        v-if="field.options"
                        v-model="form[field.key]"
                        class="coolify-input"
                        :required="field.required"
                    >
                        <option
                            v-for="option in field.options"
                            :key="option"
                            :value="option"
                        >
                            {{ option }}
                        </option></select
                    ><textarea
                        v-else-if="field.type === 'textarea'"
                        v-model="form[field.key]"
                        class="coolify-input"
                        rows="8"
                        :required="field.required" /><input
                        v-else
                        v-model="form[field.key]"
                        class="coolify-input"
                        :type="field.type ?? 'text'"
                        :required="field.required || (!editing && field.type === 'password')"
                        :placeholder="
                            editing && field.type === 'password'
                                ? 'Leave empty to keep the current value'
                                : undefined
                        "
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
                    {{ editing ? 'Save' : 'Create' }}
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
                <h2>Delete {{ config.singular }}?</h2>
                <p>This action cannot be undone.</p>
                <label class="form-field"
                    >Confirm by entering {{ editing?.name
                    }}<input
                        v-model="confirmation"
                        class="coolify-input"
                        required
                /></label>
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
                        @click="deleteDialog?.close()"
                    >
                        Cancel</button
                    ><button
                        class="coolify-btn danger-button"
                        :disabled="saving || confirmation !== editing?.name"
                    >
                        Permanently Delete
                    </button>
                </div>
            </form>
        </dialog>
    </section>
</template>
