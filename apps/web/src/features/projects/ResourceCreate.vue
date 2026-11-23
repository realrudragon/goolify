<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import AppIcon from '../../components/AppIcon.vue';
import SettingsSection from '../../components/SettingsSection.vue';
import { apiRequest } from '../../lib/api';
import { navigateTo } from '../../lib/navigation';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const props = defineProps<{ projectUuid: string; environmentUuid: string }>();
interface Choice {
    type: string;
    name: string;
    category: string;
    family: string;
    slogan?: string;
}
const applications: Choice[] = [
    {
        type: 'public',
        name: 'Public Repository',
        category: 'Applications',
        family: 'applications',
        slogan: 'Deploy a public Git repository.',
    },
    {
        type: 'private-github-app',
        name: 'Private Repository (with GitHub App)',
        category: 'Applications',
        family: 'applications',
    },
    {
        type: 'private-deploy-key',
        name: 'Private Repository (with Deploy Key)',
        category: 'Applications',
        family: 'applications',
    },
    { type: 'dockerfile', name: 'Dockerfile', category: 'Applications', family: 'applications' },
    { type: 'dockerimage', name: 'Docker Image', category: 'Applications', family: 'applications' },
    {
        type: 'dockercompose',
        name: 'Docker Compose',
        category: 'Applications',
        family: 'applications',
    },
];
const databases: Choice[] = [
    ['postgresql', 'PostgreSQL'],
    ['mysql', 'MySQL'],
    ['mariadb', 'MariaDB'],
    ['mongodb', 'MongoDB'],
    ['redis', 'Redis'],
    ['clickhouse', 'ClickHouse'],
    ['keydb', 'KeyDB'],
    ['dragonfly', 'Dragonfly'],
    ['sqlite', 'SQLite'],
].map(([type, name]) => ({ type, name, category: 'Databases', family: 'databases' }));
const lifecycle = usePageLifecycle();
const templates = ref<Choice[]>([]);
const servers = ref<{ uuid: string; name: string; is_usable: boolean }[]>([]);
const destinations = ref<{ uuid: string; name: string; server_uuid: string }[]>([]);
const selected = ref<Choice>();
const step = ref<'catalogue' | 'server' | 'form'>('catalogue');
const search = ref('');
const category = ref('');
const server = ref('');
const destination = ref('');
const form = ref<Record<string, any>>({
    name: '',
    fqdn: '',
    git_repository: '',
    git_branch: 'main',
    build_pack: 'dockerfile',
    base_directory: '/',
    publish_directory: '',
    dockerfile: '',
    docker_compose_raw: '',
    docker_registry_image_name: '',
    docker_registry_image_tag: '',
    ports_exposes: '',
    ports_mappings: '',
    image: '',
    password: '',
    private_key_uuid: '',
    github_app_uuid: '',
});
const keys = ref<{ uuid: string; name?: string }[]>([]);
const githubApps = ref<{ uuid: string; name: string }[]>([]);
const error = ref('');
const loading = ref(true);
const busy = ref(false);
const choices = computed(() =>
    [...applications, ...databases, ...templates.value].filter(
        (c) =>
            (!category.value || category.value === c.category || category.value === c.family) &&
            `${c.name} ${c.slogan ?? ''}`.toLowerCase().includes(search.value.trim().toLowerCase()),
    ),
);
const serverDestinations = computed(() =>
    destinations.value.filter((d) => d.server_uuid === server.value),
);
async function load() {
    try {
        const result = await Promise.all([
            apiRequest<Choice[]>('/api/v1/services/templates', { signal: lifecycle.signal }),
            apiRequest<typeof servers.value>('/api/v1/servers', { signal: lifecycle.signal }),
            apiRequest<typeof destinations.value>('/api/v1/destinations', {
                signal: lifecycle.signal,
            }),
        ]);
        templates.value = result[0].map((c) => ({
            ...c,
            category: 'Services',
            family: 'services',
        }));
        servers.value = result[1];
        destinations.value = result[2];
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        loading.value = false;
    }
}
function choose(choice: Choice) {
    selected.value = choice;
    step.value = 'server';
}
async function selectServer(uuid: string) {
    server.value = uuid;
    destination.value = destinations.value.find((d) => d.server_uuid === uuid)?.uuid ?? '';
    step.value = 'form';
    try {
        if (selected.value?.type === 'private-deploy-key')
            keys.value = await apiRequest('/api/v1/security/keys', { signal: lifecycle.signal });
        if (selected.value?.type === 'private-github-app')
            githubApps.value = await apiRequest('/api/v1/github-apps', {
                signal: lifecycle.signal,
            });
    } catch (err) {
        error.value = (err as Error).message;
    }
}
async function create() {
    if (!selected.value || busy.value) return;
    busy.value = true;
    error.value = '';
    try {
        const choice = selected.value;
        const payload: Record<string, any> = {
            name: form.value.name,
            project_uuid: props.projectUuid,
            environment_uuid: props.environmentUuid,
            server_uuid: server.value,
            destination_uuid: destination.value,
        };
        if (choice.family === 'services') {
            payload.type = choice.type;
            if (form.value.fqdn) payload.fqdn = form.value.fqdn;
        } else if (choice.family === 'databases') {
            if (form.value.image) payload.image = form.value.image;
            if (form.value.password) payload.password = form.value.password;
        } else {
            const fields =
                choice.type === 'dockerfile'
                    ? ['dockerfile']
                    : choice.type === 'dockercompose'
                      ? ['docker_compose_raw']
                      : choice.type === 'dockerimage'
                        ? ['docker_registry_image_name', 'docker_registry_image_tag']
                        : [
                              'git_repository',
                              'git_branch',
                              ...(choice.type === 'private-github-app'
                                  ? ['github_app_uuid']
                                  : choice.type === 'private-deploy-key'
                                    ? ['private_key_uuid']
                                    : []),
                          ];
            for (const key of [
                ...fields,
                'fqdn',
                'ports_exposes',
                'ports_mappings',
                'base_directory',
                'publish_directory',
            ])
                if (form.value[key] !== '') payload[key] = form.value[key];
            if (['public', 'private-github-app', 'private-deploy-key'].includes(choice.type))
                payload.build_pack = form.value.build_pack || 'dockerfile';
        }
        const created = await apiRequest<{ uuid: string }>(
            `/api/v1/${choice.family}${choice.family === 'services' ? '' : `/${choice.type}`}`,
            { method: 'POST', body: JSON.stringify(payload) },
        );
        navigateTo(
            `/project/${props.projectUuid}/environment/${props.environmentUuid}/${choice.family === 'applications' ? 'application' : choice.family === 'databases' ? 'database' : 'service'}/${created.uuid}`,
        );
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        busy.value = false;
    }
}
onMounted(load);
</script>

<template>
    <section
        class="original-page"
        data-testid="new-resource-page"
    >
        <p
            v-if="error"
            role="alert"
            class="page-error"
        >
            {{ error }}
        </p>
        <template v-if="step === 'catalogue'"
            ><div class="list-controls">
                <div class="list-search">
                    <AppIcon name="search" /><input
                        v-model="search"
                        type="search"
                        placeholder="Search resources"
                        aria-label="Search resources"
                    />
                </div>
                <select
                    v-model="category"
                    class="coolify-input"
                    aria-label="Category"
                >
                    <option value="">All categories</option>
                    <option>Applications</option>
                    <option>Databases</option>
                    <option>Services</option>
                </select>
            </div>
            <p
                v-if="loading"
                role="status"
            >
                Loading resources…
            </p>
            <template
                v-for="group in ['Applications', 'Databases', 'Services']"
                :key="group"
                ><template v-if="choices.some((c) => c.category === group)"
                    ><h2 class="catalogue-heading">{{ group }}</h2>
                    <div class="original-card-grid">
                        <button
                            v-for="choice in choices.filter((c) => c.category === group)"
                            :key="`${choice.family}/${choice.type}`"
                            class="original-resource-card catalogue-choice"
                            type="button"
                            @click="choose(choice)"
                        >
                            <span class="resource-glyph"
                                ><AppIcon
                                    :name="
                                        group === 'Applications'
                                            ? 'code'
                                            : group === 'Databases'
                                              ? 'layers'
                                              : 'projects'
                                    " /></span
                            ><strong>{{ choice.name }}</strong>
                            <p>{{ choice.slogan }}</p>
                        </button>
                    </div></template
                ></template
            ></template
        >
        <template v-else-if="step === 'server'"
            ><div class="section-heading">
                <h2>Select a server</h2>
                <button
                    type="button"
                    class="coolify-btn"
                    @click="step = 'catalogue'"
                >
                    Back
                </button>
            </div>
            <div class="original-card-grid">
                <button
                    v-for="host in servers"
                    :key="host.uuid"
                    type="button"
                    class="original-resource-card catalogue-choice"
                    @click="selectServer(host.uuid)"
                >
                    <span class="resource-glyph"><AppIcon name="servers" /></span
                    ><strong>{{ host.name }}</strong
                    ><small>{{ host.is_usable ? 'Ready' : 'Validation required' }}</small>
                </button>
            </div>
            <p
                v-if="!servers.length"
                class="original-empty"
            >
                No servers available. <a href="/servers/new">Add a server</a>
            </p></template
        >
        <form
            v-else
            @submit.prevent="create"
        >
            <SettingsSection :title="`New ${selected?.name}`"
                ><template #actions
                    ><button
                        type="button"
                        class="coolify-btn"
                        @click="step = 'server'"
                    >
                        Back</button
                    ><button
                        type="submit"
                        class="coolify-btn coolify-btn-primary"
                        :disabled="busy"
                    >
                        Continue
                    </button></template
                >
                <div class="form-grid">
                    <label class="form-field"
                        >Name<input
                            v-model="form.name"
                            class="coolify-input"
                            required /></label
                    ><label class="form-field"
                        >Destination<select
                            v-model="destination"
                            class="coolify-input"
                            required
                        >
                            <option
                                value=""
                                disabled
                            >
                                Select destination
                            </option>
                            <option
                                v-for="network in serverDestinations"
                                :key="network.uuid"
                                :value="network.uuid"
                            >
                                {{ network.name }}
                            </option>
                        </select></label
                    >
                    <template v-if="selected?.family === 'applications'">
                        <template
                            v-if="
                                ['public', 'private-github-app', 'private-deploy-key'].includes(
                                    selected.type,
                                )
                            "
                            ><label class="form-field full-span"
                                >Repository URL<input
                                    v-model="form.git_repository"
                                    class="coolify-input"
                                    required /></label
                            ><label class="form-field"
                                >Branch<input
                                    v-model="form.git_branch"
                                    class="coolify-input"
                                    required /></label
                            ><label class="form-field"
                                >Build Pack<select
                                    v-model="form.build_pack"
                                    class="coolify-input"
                                >
                                    <option value="dockerfile">Dockerfile</option>
                                    <option value="railpack">Railpack</option>
                                    <option value="dockercompose">Docker Compose</option>
                                    <option value="static">Static</option>
                                </select></label
                            ><label class="form-field"
                                >Base Directory<input
                                    v-model="form.base_directory"
                                    class="coolify-input"
                                    placeholder="/" /></label
                            ><label
                                v-if="form.build_pack === 'static'"
                                class="form-field"
                                >Publish Directory<input
                                    v-model="form.publish_directory"
                                    class="coolify-input"
                                    placeholder="/dist" /></label
                            ><label
                                v-if="selected.type === 'private-deploy-key'"
                                class="form-field"
                                >Deploy key<select
                                    v-model="form.private_key_uuid"
                                    class="coolify-input"
                                    required
                                >
                                    <option
                                        v-for="key in keys"
                                        :key="key.uuid"
                                        :value="key.uuid"
                                    >
                                        {{ key.name ?? key.uuid }}
                                    </option>
                                </select></label
                            ><label
                                v-if="selected.type === 'private-github-app'"
                                class="form-field"
                                >GitHub App<select
                                    v-model="form.github_app_uuid"
                                    class="coolify-input"
                                    required
                                >
                                    <option
                                        v-for="app in githubApps"
                                        :key="app.uuid"
                                        :value="app.uuid"
                                    >
                                        {{ app.name }}
                                    </option>
                                </select></label
                            ></template
                        >
                        <label
                            v-if="selected.type === 'dockerfile'"
                            class="form-field full-span"
                            >Dockerfile<textarea
                                v-model="form.dockerfile"
                                class="coolify-input code-editor"
                                rows="12"
                                required
                            /></label
                        ><label
                            v-if="selected.type === 'dockercompose'"
                            class="form-field full-span"
                            >Docker Compose<textarea
                                v-model="form.docker_compose_raw"
                                class="coolify-input code-editor"
                                rows="12"
                                required
                            /></label
                        ><template v-if="selected.type === 'dockerimage'"
                            ><label class="form-field"
                                >Docker Image<input
                                    v-model="form.docker_registry_image_name"
                                    class="coolify-input"
                                    required /></label
                            ><label class="form-field"
                                >Tag<input
                                    v-model="form.docker_registry_image_tag"
                                    class="coolify-input" /></label></template
                        ><label class="form-field full-span"
                            >Domains (FQDN)<input
                                v-model="form.fqdn"
                                class="coolify-input"
                                placeholder="https://app.example.com" /></label
                        ><label class="form-field"
                            >Ports exposes<input
                                v-model="form.ports_exposes"
                                class="coolify-input"
                                placeholder="80" /></label
                        ><label class="form-field"
                            >Ports mappings<input
                                v-model="form.ports_mappings"
                                class="coolify-input"
                                placeholder="8080:80"
                        /></label>
                    </template>
                    <template v-if="selected?.family === 'services'"
                        ><label class="form-field full-span"
                            >Domains (FQDN)<input
                                v-model="form.fqdn"
                                class="coolify-input"
                                placeholder="https://service.example.com"
                        /></label>
                    </template>
                    <template v-if="selected?.family === 'databases'"
                        ><label class="form-field"
                            >Image<input
                                v-model="form.image"
                                class="coolify-input"
                                placeholder="Use default image" /></label
                        ><label class="form-field"
                            >Password<input
                                v-model="form.password"
                                class="coolify-input"
                                type="password"
                                autocomplete="new-password"
                                placeholder="Generate automatically" /></label
                    ></template></div
            ></SettingsSection>
        </form>
    </section>
</template>
