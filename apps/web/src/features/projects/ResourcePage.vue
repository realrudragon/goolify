<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue';
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
import { navigateTo } from '../../lib/navigation';
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
const envComment = ref('');
const envPreview = ref(false);
const envMultiline = ref(false);
const envLiteral = ref(false);
const envBuildtime = ref(true);
const envRuntime = ref(true);
const envViewMode = ref<'normal' | 'developer'>('normal');
const envSearch = ref('');
const envFilter = ref('');
const devProdText = ref('');
const devPreviewText = ref('');
const domainSearch = ref('');
const domainDialog = ref<HTMLDialogElement>();
const domainForm = ref({
    index: -1,
    url: '',
    port: '',
    forceHttps: true,
    redirect: 'both',
});
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
    ['is_env_sorting_enabled', 'Sort Environment Variables Alphabetically'],
    ['use_build_secrets', 'Use Docker BuildKit Secrets'],
    ['include_coolify_default_variables', 'Include Coolify Default Variables'],
    ['inject_build_args_to_dockerfile', 'Inject Build Args Automatically'],
    ['include_source_commit_in_build', 'Include Source Commit in Build'],
    ['include_network_hosts_in_build', 'Add Network Containers as Hosts During Build'],
    ['is_gzip_enabled', 'Gzip Compression'],
    ['is_stripprefix_enabled', 'Strip Path Prefixes'],
    ['is_permanent_redirect_enabled', 'Permanent Redirect (301/308)'],
    ['is_consistent_container_name_enabled', 'Consistent Container Name'],
    ['is_include_timestamps', 'Include Timestamps in Logs'],
    ['connect_to_docker_network', 'Connect to Predefined Network'],
    ['is_gpu_enabled', 'GPU Support'],
];
const endpoint = computed(
    () =>
        `/api/v1/${props.kind === 'application' ? 'applications' : props.kind === 'service' ? 'services' : 'databases'}/${encodeURIComponent(props.uuid)}`,
);
const base = computed(
    () =>
        `/project/${props.projectUuid ?? resource.value?.project_uuid}/environment/${props.environmentUuid ?? resource.value?.environment_uuid}/${props.kind}/${props.uuid}`,
);
const section = computed(() => (props.subpage || 'general').split('/')[0] || 'general');
const selectedDeploymentUuid = computed(() =>
    (props.subpage || '').startsWith('deployment/')
        ? props.subpage.slice('deployment/'.length)
        : '',
);
const isFullWidthTab = computed(() => ['deployment', 'logs', 'terminal'].includes(section.value));
const domainLinks = computed(() =>
    String(resource.value?.fqdn ?? form.value?.fqdn ?? '')
        .split(',')
        .map((d) => d.trim())
        .filter(Boolean)
        .map((d) => (d.includes('://') ? d : `http://${d}`)),
);
const domainRows = computed(() => {
    const q = domainSearch.value.trim().toLowerCase();
    const defaultPort = String(form.value.ports_exposes || '80').split(',')[0].trim() || '80';
    return String(form.value.fqdn ?? '')
        .split(',')
        .map((raw, index) => {
            const trimmed = raw.trim();
            if (!trimmed) return null;
            const normalized = trimmed.includes('://') ? trimmed : `http://${trimmed}`;
            let port = defaultPort;
            let scheme = 'http';
            let displayUrl = normalized;
            try {
                const u = new URL(normalized);
                scheme = u.protocol.replace(':', '');
                if (u.port) port = u.port;
                displayUrl = `${u.protocol}//${u.hostname}${u.pathname === '/' ? '' : u.pathname}`;
            } catch {
                // keep fallback
            }
            return {
                index,
                raw: trimmed,
                url: displayUrl,
                scheme,
                port,
                protocolRedirect:
                    scheme === 'https' && advancedSettings.value.is_force_https_enabled
                        ? 'Force HTTPS'
                        : 'Disabled',
                domainRedirect:
                    form.value.redirect === 'non-www'
                        ? 'www to non-www'
                        : form.value.redirect === 'www'
                          ? 'non-www to www'
                          : 'Disabled',
            };
        })
        .filter((row): row is NonNullable<typeof row> => Boolean(row))
        .filter((row) => !q || row.url.toLowerCase().includes(q) || row.port.includes(q));
});
const filteredEnvRows = computed(() => {
    const q = envSearch.value.trim().toLowerCase();
    let list = rows.value.filter((row) => {
        if (q && !String(row.key ?? '').toLowerCase().includes(q)) return false;
        if (envFilter.value === 'production' && row.is_preview) return false;
        if (envFilter.value === 'preview' && !row.is_preview) return false;
        if (envFilter.value === 'buildtime' && !row.is_buildtime) return false;
        if (envFilter.value === 'runtime' && !row.is_runtime) return false;
        if (envFilter.value === 'literal' && !row.is_literal) return false;
        return true;
    });
    if (advancedSettings.value.is_env_sorting_enabled) {
        list = [...list].sort((a, b) => String(a.key ?? '').localeCompare(String(b.key ?? '')));
    }
    return list;
});
const prodEnvRows = computed(() => filteredEnvRows.value.filter((r) => !r.is_preview));
const previewEnvRows = computed(() => filteredEnvRows.value.filter((r) => r.is_preview));
function syncDeveloperText() {
    const formatRows = (list: Record<string, any>[]) =>
        list
            .map((r) => {
                const flags: string[] = [];
                if (r.is_literal) flags.push('literal');
                if (r.is_buildtime === false) flags.push('buildtime=false');
                if (r.is_runtime === false) flags.push('runtime=false');
                const prefix = flags.length ? `# coolify: ${flags.join(' ')}\n` : '';
                return `${prefix}${r.key}=${r.value ?? ''}`;
            })
            .join('\n');
    devProdText.value = formatRows(rows.value.filter((r) => !r.is_preview));
    devPreviewText.value = formatRows(rows.value.filter((r) => r.is_preview));
}
function toggleEnvViewMode() {
    if (envViewMode.value === 'normal') {
        syncDeveloperText();
        envViewMode.value = 'developer';
    } else {
        envViewMode.value = 'normal';
    }
}
function openAddEnv() {
    envKey.value = '';
    envValue.value = '';
    envComment.value = '';
    envPreview.value = false;
    envMultiline.value = false;
    envLiteral.value = false;
    envBuildtime.value = true;
    envRuntime.value = true;
    envDialog.value?.showModal();
}
function openEditEnv(row: Record<string, any>) {
    envKey.value = row.key ?? '';
    envValue.value = row.value ?? '';
    envComment.value = '';
    envPreview.value = Boolean(row.is_preview);
    envMultiline.value = String(row.value ?? '').includes('\n');
    envLiteral.value = Boolean(row.is_literal);
    envBuildtime.value = row.is_buildtime !== false;
    envRuntime.value = row.is_runtime !== false;
    envDialog.value?.showModal();
}
async function copyEnvValue(val: string) {
    try {
        await navigator.clipboard.writeText(val ?? '');
        pushToast('Copied to clipboard.', 'success');
    } catch {
        // ignore clipboard error
    }
}
function parseDeveloperEnvBlock(text: string, isPreview: boolean) {
    const entries: Record<string, any>[] = [];
    let pendingFlags: { literal?: boolean; buildtime?: boolean; runtime?: boolean } = {};
    for (const rawLine of text.split(/\r?\n/)) {
        const line = rawLine.trim();
        if (!line) {
            pendingFlags = {};
            continue;
        }
        if (line.startsWith('# coolify:')) {
            const spec = line.slice('# coolify:'.length);
            pendingFlags = {
                literal: /\bliteral\b/.test(spec),
                buildtime: !/\bbuildtime=false\b/.test(spec),
                runtime: !/\bruntime=false\b/.test(spec),
            };
            continue;
        }
        if (line.startsWith('#')) continue;
        const eq = line.indexOf('=');
        if (eq <= 0) continue;
        const key = line.slice(0, eq).trim();
        const val = line.slice(eq + 1);
        entries.push({
            key,
            value: val,
            is_preview: isPreview,
            is_literal: Boolean(pendingFlags.literal),
            is_buildtime: pendingFlags.buildtime !== false,
            is_runtime: pendingFlags.runtime !== false,
        });
        pendingFlags = {};
    }
    return entries;
}
async function saveDeveloperView() {
    if (!canEdit.value || busy.value) return;
    busy.value = true;
    error.value = '';
    try {
        const data = [
            ...parseDeveloperEnvBlock(devProdText.value, false),
            ...parseDeveloperEnvBlock(devPreviewText.value, true),
        ];
        if (props.kind === 'application') {
            await apiRequest(`${endpoint.value}/envs/bulk`, {
                method: 'POST',
                body: JSON.stringify({ data }),
            });
        } else {
            for (const item of data) {
                await apiRequest(`${endpoint.value}/envs`, {
                    method: 'POST',
                    body: JSON.stringify(item),
                });
            }
        }
        await load();
        status.value = 'Environment variables saved.';
        pushToast('Environment variables saved.', 'success');
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        busy.value = false;
    }
}
function openDomainModal(row?: (typeof domainRows.value)[number]) {
    if (row) {
        domainForm.value = {
            index: row.index,
            url: row.url,
            port: row.port,
            forceHttps: advancedSettings.value.is_force_https_enabled !== false,
            redirect: form.value.redirect || 'both',
        };
    } else {
        domainForm.value = {
            index: -1,
            url: '',
            port: String(form.value.ports_exposes || '80').split(',')[0].trim() || '80',
            forceHttps: advancedSettings.value.is_force_https_enabled !== false,
            redirect: form.value.redirect || 'both',
        };
    }
    domainDialog.value?.showModal();
}
async function generateDomainIntoModal() {
    await generateDomain();
    domainForm.value.url = form.value.fqdn;
}
async function saveDomainModal() {
    if (!canEdit.value || busy.value) return;
    let urlVal = domainForm.value.url.trim();
    if (!urlVal) return;
    if (!urlVal.includes('://')) urlVal = `https://${urlVal}`;
    const portVal = domainForm.value.port.trim();
    const defaultPort = String(form.value.ports_exposes || '80').split(',')[0].trim() || '80';
    let entry = urlVal;
    if (portVal && portVal !== defaultPort) {
        try {
            const u = new URL(urlVal);
            u.port = portVal;
            entry = u.toString().replace(/\/$/, '');
        } catch {
            entry = `${urlVal}:${portVal}`;
        }
    }
    const list = String(form.value.fqdn ?? '')
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean);
    if (domainForm.value.index >= 0 && domainForm.value.index < list.length) {
        list[domainForm.value.index] = entry;
    } else {
        list.push(entry);
    }
    form.value.fqdn = list.join(',');
    if (props.kind === 'application') {
        form.value.redirect = domainForm.value.redirect || 'both';
        advancedSettings.value.is_force_https_enabled = domainForm.value.forceHttps;
    }
    domainDialog.value?.close();
    await save();
}
async function deleteDomainRow(idx: number) {
    if (!canEdit.value || busy.value) return;
    const list = String(form.value.fqdn ?? '')
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean);
    list.splice(idx, 1);
    form.value.fqdn = list.join(',');
    await save();
}
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
                      ports_mappings: item.ports_mappings ?? '',
                      redirect: item.redirect ?? 'both',
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
                        fqdn: item.fqdn ?? '',
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
        if (section.value === 'environment-variables') {
            rows.value = await apiRequest(`${endpoint.value}/envs`, { signal: lifecycle.signal });
            syncDeveloperText();
        }
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
        if ((section.value === 'metrics' || section.value === 'analytics') && item.server_uuid) {
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
async function generateDomain() {
    if (!canEdit.value || !resource.value) return;
    try {
        const srvUuid = resource.value.server_uuid || 'localhost';
        const srv = await apiRequest<{ ip?: string; wildcard_domain?: string }>(
            `/api/v1/servers/${encodeURIComponent(srvUuid)}`,
            { signal: lifecycle.signal },
        );
        const wc = (srv.wildcard_domain ?? '').trim().replace(/\/+$/, '');
        if (wc) {
            const hasScheme = wc.includes('://');
            const [scheme, rest] = hasScheme ? wc.split('://') : ['http', wc];
            const host = rest.replace(/^\*\./, '');
            form.value.fqdn = `${scheme}://${props.uuid}.${host}`;
        } else {
            let ip = (srv.ip ?? '').trim();
            if (!ip || ip === 'host.docker.internal' || ip === 'localhost') {
                ip = (typeof window !== 'undefined' && window.location.hostname) || '127.0.0.1';
            }
            form.value.fqdn = `http://${props.uuid}.${ip}.sslip.io`;
        }
    } catch {
        const host = (typeof window !== 'undefined' && window.location.hostname) || '127.0.0.1';
        form.value.fqdn = `http://${props.uuid}.${host}.sslip.io`;
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
            navigateTo(`${base.value}/deployment/${res.deployment_uuid}`);
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
            if (deployment) navigateTo(`${base.value}/deployment/${deployment}`);
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
                is_literal: envLiteral.value,
                is_buildtime: envBuildtime.value,
                is_runtime: envRuntime.value,
            }),
        });
        envKey.value = '';
        envValue.value = '';
        envComment.value = '';
        envDialog.value?.close();
        await load();
        pushToast('Environment variable saved.', 'success');
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
        navigateTo(
            `/project/${props.projectUuid}/environment/${props.environmentUuid}`,
        );
    } catch (err) {
        error.value = (err as Error).message;
        busy.value = false;
    }
}
watch(
    () => [props.kind, props.uuid, props.subpage],
    () => {
        status.value = '';
        error.value = '';
        void load();
    },
);
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
                    v-for="dUrl in domainLinks"
                    :key="dUrl"
                    :href="dUrl"
                    target="_blank"
                    rel="noopener noreferrer"
                    class="coolify-btn"
                    :title="`Open ${dUrl}`"
                    >{{ dUrl }} ↗</a
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
            class="resource-tabs sr-only"
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
            style="grid-template-columns: 1fr"
        >
            <nav
                v-if="!isFullWidthTab"
                class="settings-sidebar sr-only"
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
                                ><template #actions
                                    ><button
                                        v-if="kind === 'service'"
                                        type="button"
                                        class="coolify-btn"
                                        :disabled="!canEdit"
                                        @click="composeDialog?.showModal()"
                                    >
                                        Edit Compose File</button
                                    ><button
                                        v-if="canEdit && !(kind === 'database' && resource.id === 0)"
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
                                            :readonly="!canEdit || (kind === 'database' && resource.id === 0)" /></label
                                    ><label class="form-field"
                                        >Description<input
                                            v-model="form.description"
                                            class="coolify-input"
                                            :readonly="!canEdit || (kind === 'database' && resource.id === 0)"
                                    /></label></div></SettingsSection
                            ><SettingsSection
                                v-if="kind === 'application' || kind === 'service'"
                                title="Access"
                                helper="Configure domain routing (Traefik / Caddy) and exposed container or host ports. Separate multiple domains with commas; append :port to route a domain to a specific container port."
                                ><template #actions
                                    ><button
                                        v-if="canEdit"
                                        type="button"
                                        class="coolify-btn"
                                        :disabled="busy"
                                        @click="generateDomain"
                                    >
                                        Generate Domain</button
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
                                    <label class="form-field full-span"
                                        >Domains<input
                                            v-model="form.fqdn"
                                            class="coolify-input"
                                            placeholder="https://app.example.com,http://app.example.com:3000"
                                            :readonly="!canEdit" /></label
                                    ><template v-if="kind === 'application'"
                                        ><label class="form-field"
                                            >Direction<select
                                                v-model="form.redirect"
                                                class="coolify-input"
                                                :disabled="!canEdit"
                                            >
                                                <option value="both">Allow www &amp; non-www</option>
                                                <option value="non-www">Redirect to non-www</option>
                                                <option value="www">Redirect to www</option>
                                            </select></label
                                        ><label class="form-field"
                                            >Ports Exposes<input
                                                v-model="form.ports_exposes"
                                                class="coolify-input"
                                                placeholder="80"
                                                :readonly="!canEdit" /></label
                                        ><label class="form-field"
                                            >Ports Mappings<input
                                                v-model="form.ports_mappings"
                                                class="coolify-input"
                                                placeholder="8080:80,3000:3000"
                                                :readonly="!canEdit"
                                        /></label></template></div></SettingsSection
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
                <div
                    v-else-if="section === 'domains' && (kind === 'application' || kind === 'service')"
                    class="settings-content"
                >
                    <SettingsSection title="Domains">
                        <template #actions>
                            <input
                                v-model="domainSearch"
                                type="search"
                                class="coolify-input"
                                placeholder="Search services or domains"
                                aria-label="Search services or domains"
                                style="width: 220px; height: 32px; font-size: 13px"
                            />
                            <button
                                v-if="canEdit"
                                type="button"
                                class="coolify-btn"
                                :disabled="busy"
                                @click="
                                    generateDomain().then(() => {
                                        if (form.fqdn) void save();
                                    })
                                "
                            >
                                DNS ⌄
                            </button>
                            <button
                                v-if="canEdit"
                                type="button"
                                class="coolify-btn coolify-btn-primary"
                                :disabled="busy"
                                @click="openDomainModal()"
                            >
                                + Add Domain
                            </button>
                        </template>
                        <div class="original-table-scroll">
                            <table class="original-table">
                                <thead>
                                    <tr>
                                        <th>Domain</th>
                                        <th>Protocol redirect</th>
                                        <th>Domain redirect</th>
                                        <th>Internal port</th>
                                        <th>Search indexing</th>
                                        <th>DNS status</th>
                                        <th>Actions</th>
                                    </tr>
                                </thead>
                                <tbody>
                                    <tr
                                        v-for="d in domainRows"
                                        :key="d.index + d.raw"
                                    >
                                        <td>
                                            <a
                                                :href="d.url"
                                                target="_blank"
                                                rel="noopener noreferrer"
                                                class="code-text"
                                                >{{ d.url }}</a
                                            >
                                        </td>
                                        <td>{{ d.protocolRedirect }}</td>
                                        <td>{{ d.domainRedirect }}</td>
                                        <td class="code-text">{{ d.port }}</td>
                                        <td>✓</td>
                                        <td>
                                            <span
                                                style="
                                                    display: inline-flex;
                                                    align-items: center;
                                                    gap: 6px;
                                                "
                                            >
                                                <span
                                                    class="status-dot"
                                                    style="background: #10b981"
                                                ></span>
                                                DNS matches
                                            </span>
                                        </td>
                                        <td>
                                            <div style="display: flex; gap: 6px">
                                                <button
                                                    v-if="canEdit"
                                                    type="button"
                                                    class="coolify-btn"
                                                    @click="openDomainModal(d)"
                                                >
                                                    Edit
                                                </button>
                                                <button
                                                    v-if="canEdit"
                                                    type="button"
                                                    class="coolify-btn danger-button"
                                                    @click="deleteDomainRow(d.index)"
                                                >
                                                    Delete
                                                </button>
                                            </div>
                                        </td>
                                    </tr>
                                </tbody>
                            </table>
                        </div>
                        <p
                            v-if="!domainRows.length"
                            class="page-muted"
                        >
                            No domains configured yet. Click "+ Add Domain" or "DNS" to generate a
                            domain.
                        </p>
                    </SettingsSection>

                    <form
                        v-if="kind === 'application'"
                        @submit.prevent="save"
                    >
                        <SettingsSection title="Redirects">
                            <template #actions>
                                <button
                                    v-if="canEdit"
                                    type="submit"
                                    class="coolify-btn coolify-btn-primary"
                                    :disabled="busy"
                                >
                                    Save
                                </button>
                            </template>
                            <div class="form-grid">
                                <label class="form-field">
                                    Redirect type
                                    <select
                                        v-model="advancedSettings.is_permanent_redirect_enabled"
                                        class="coolify-input"
                                        :disabled="!canEdit"
                                    >
                                        <option :value="false">Temporary</option>
                                        <option :value="true">Permanent (301/308)</option>
                                    </select>
                                </label>
                            </div>
                        </SettingsSection>
                    </form>
                </div>

                <div
                    v-else-if="section === 'environment-variables'"
                    class="settings-content"
                >
                    <SettingsSection title="Environment variables">
                        <template #actions>
                            <button
                                v-if="canEdit"
                                type="button"
                                class="coolify-btn"
                                @click="toggleEnvViewMode"
                            >
                                {{ envViewMode === 'normal' ? 'Developer View' : 'Normal View' }}
                            </button>
                        </template>

                        <template v-if="envViewMode === 'normal'">
                            <form
                                v-if="kind === 'application'"
                                class="coolify-well"
                                style="display: grid; gap: 14px; margin-bottom: 16px"
                                @change="save"
                            >
                                <div class="form-grid">
                                    <label class="form-field">
                                        Environment variable order
                                        <select
                                            v-model="advancedSettings.is_env_sorting_enabled"
                                            class="coolify-input"
                                            :disabled="!canEdit"
                                        >
                                            <option :value="false">Creation order</option>
                                            <option :value="true">Alphabetical</option>
                                        </select>
                                    </label>
                                    <label class="form-field">
                                        Build secrets
                                        <select
                                            v-model="advancedSettings.use_build_secrets"
                                            class="coolify-input"
                                            :disabled="!canEdit"
                                        >
                                            <option :value="false">Standard build arguments</option>
                                            <option :value="true">Docker BuildKit secrets</option>
                                        </select>
                                    </label>
                                </div>
                                <label
                                    class="check-field"
                                    style="justify-content: space-between"
                                >
                                    <span>Include Coolify default variables</span>
                                    <input
                                        v-model="advancedSettings.include_coolify_default_variables"
                                        type="checkbox"
                                        :disabled="!canEdit"
                                    />
                                </label>
                                <div class="form-grid">
                                    <label class="form-field">
                                        Environment values in logs
                                        <select
                                            v-model="advancedSettings.redact_all_env_values_in_logs"
                                            class="coolify-input"
                                            :disabled="!canEdit"
                                        >
                                            <option :value="true">Hide all values</option>
                                            <option :value="false">Hide locked values only</option>
                                        </select>
                                    </label>
                                </div>
                            </form>

                            <div class="env-toolbar">
                                <input
                                    v-model="envSearch"
                                    type="search"
                                    class="coolify-input"
                                    placeholder="Search environment variables"
                                    aria-label="Search environment variables"
                                    style="max-width: 320px"
                                />
                                <div style="display: flex; gap: 8px; align-items: center">
                                    <select
                                        v-model="envFilter"
                                        class="coolify-input"
                                        aria-label="Filter environment variables"
                                        style="width: auto"
                                    >
                                        <option value="">Filter</option>
                                        <option value="production">Production</option>
                                        <option value="preview">Preview</option>
                                        <option value="buildtime">Buildtime</option>
                                        <option value="runtime">Runtime</option>
                                        <option value="literal">Literal</option>
                                    </select>
                                    <button
                                        v-if="canEdit"
                                        type="button"
                                        class="coolify-btn coolify-btn-primary"
                                        @click="openAddEnv"
                                    >
                                        <AppIcon name="plus" />Add
                                    </button>
                                </div>
                            </div>

                            <div class="original-table-scroll">
                                <table class="original-table">
                                    <thead>
                                        <tr>
                                            <th>Name</th>
                                            <th>Managed</th>
                                            <th>Literal</th>
                                            <th>Multiline</th>
                                            <th>Buildtime</th>
                                            <th>Runtime</th>
                                            <th>Actions</th>
                                        </tr>
                                    </thead>
                                    <tbody>
                                        <tr
                                            v-if="prodEnvRows.length"
                                            class="env-group-row"
                                        >
                                            <td colspan="7"><strong>Production</strong></td>
                                        </tr>
                                        <tr
                                            v-for="row in prodEnvRows"
                                            :key="row.uuid"
                                        >
                                            <td class="code-text">{{ row.key }}</td>
                                            <td>-</td>
                                            <td>{{ row.is_literal ? '✓' : '-' }}</td>
                                            <td>
                                                {{
                                                    String(row.value ?? '').includes('\n')
                                                        ? '✓'
                                                        : '-'
                                                }}
                                            </td>
                                            <td>{{ row.is_buildtime !== false ? '✓' : '-' }}</td>
                                            <td>{{ row.is_runtime !== false ? '✓' : '-' }}</td>
                                            <td>
                                                <div style="display: flex; gap: 6px">
                                                    <button
                                                        type="button"
                                                        class="coolify-btn"
                                                        title="Copy value"
                                                        @click="copyEnvValue(row.value ?? '')"
                                                    >
                                                        Copy
                                                    </button>
                                                    <button
                                                        v-if="canEdit"
                                                        type="button"
                                                        class="coolify-btn"
                                                        title="Edit variable"
                                                        @click="openEditEnv(row)"
                                                    >
                                                        Edit
                                                    </button>
                                                    <button
                                                        v-if="canEdit"
                                                        type="button"
                                                        class="coolify-btn danger-button"
                                                        @click="removeEnv(row.uuid)"
                                                    >
                                                        Delete
                                                    </button>
                                                </div>
                                            </td>
                                        </tr>
                                        <tr
                                            v-if="previewEnvRows.length"
                                            class="env-group-row"
                                        >
                                            <td colspan="7"><strong>Preview deployments</strong></td>
                                        </tr>
                                        <tr
                                            v-for="row in previewEnvRows"
                                            :key="row.uuid"
                                        >
                                            <td class="code-text">{{ row.key }}</td>
                                            <td>-</td>
                                            <td>{{ row.is_literal ? '✓' : '-' }}</td>
                                            <td>
                                                {{
                                                    String(row.value ?? '').includes('\n')
                                                        ? '✓'
                                                        : '-'
                                                }}
                                            </td>
                                            <td>{{ row.is_buildtime !== false ? '✓' : '-' }}</td>
                                            <td>{{ row.is_runtime !== false ? '✓' : '-' }}</td>
                                            <td>
                                                <div style="display: flex; gap: 6px">
                                                    <button
                                                        type="button"
                                                        class="coolify-btn"
                                                        @click="copyEnvValue(row.value ?? '')"
                                                    >
                                                        Copy
                                                    </button>
                                                    <button
                                                        v-if="canEdit"
                                                        type="button"
                                                        class="coolify-btn"
                                                        @click="openEditEnv(row)"
                                                    >
                                                        Edit
                                                    </button>
                                                    <button
                                                        v-if="canEdit"
                                                        type="button"
                                                        class="coolify-btn danger-button"
                                                        @click="removeEnv(row.uuid)"
                                                    >
                                                        Delete
                                                    </button>
                                                </div>
                                            </td>
                                        </tr>
                                    </tbody>
                                </table>
                            </div>
                            <p
                                v-if="!filteredEnvRows.length"
                                class="page-muted"
                            >
                                No environment variables found
                            </p>
                        </template>

                        <template v-else>
                            <div class="coolify-info-callout">
                                <strong>Note</strong>
                                <p>
                                    Inline comments with space before # (e.g., KEY=value #comment)
                                    are stripped. To set flags, put
                                    <code># coolify: literal buildtime=false runtime=false</code>
                                    directly above a variable line. Without this line, the flags of
                                    a variable do not change.
                                </p>
                            </div>
                            <form
                                class="settings-content"
                                @submit.prevent="saveDeveloperView"
                            >
                                <label class="form-field full-span">
                                    Production
                                    <textarea
                                        v-model="devProdText"
                                        class="coolify-input code-editor"
                                        rows="12"
                                        :readonly="!canEdit"
                                    />
                                </label>
                                <label class="form-field full-span">
                                    Preview deployments
                                    <textarea
                                        v-model="devPreviewText"
                                        class="coolify-input code-editor"
                                        rows="6"
                                        :readonly="!canEdit"
                                    />
                                </label>
                                <div class="page-actions">
                                    <button
                                        v-if="canEdit"
                                        type="submit"
                                        class="coolify-btn coolify-btn-primary"
                                        :disabled="busy"
                                    >
                                        Save
                                    </button>
                                </div>
                            </form>
                        </template>
                    </SettingsSection>
                </div>
                <DeploymentHistory
                    v-else-if="section === 'deployment'"
                    :application-uuid="uuid"
                    :selected-deployment-uuid="selectedDeploymentUuid"
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
                    class="settings-content"
                    @submit.prevent="save"
                >
                    <SettingsSection
                        id="advanced-build-section"
                        title="Build &amp; deploy"
                    >
                        <template #actions>
                            <button
                                v-if="canEdit"
                                type="submit"
                                class="coolify-btn coolify-btn-primary"
                                :disabled="busy"
                            >
                                Save
                            </button>
                        </template>
                        <div class="form-grid">
                            <label class="form-field">
                                Build cache
                                <select
                                    v-model="advancedSettings.disable_build_cache"
                                    class="coolify-input"
                                    :disabled="!canEdit"
                                >
                                    <option :value="false">Use Docker build cache</option>
                                    <option :value="true">Rebuild from scratch every time</option>
                                </select>
                            </label>
                            <label class="form-field">
                                Build arguments
                                <select
                                    v-model="advancedSettings.inject_build_args_to_dockerfile"
                                    class="coolify-input"
                                    :disabled="!canEdit"
                                >
                                    <option :value="true">Inject build args automatically</option>
                                    <option :value="false">Managed manually in Dockerfile</option>
                                </select>
                            </label>
                            <label class="form-field">
                                Source commit availability
                                <select
                                    v-model="advancedSettings.include_source_commit_in_build"
                                    class="coolify-input"
                                    :disabled="!canEdit"
                                >
                                    <option :value="false">Runtime only (preserves cache)</option>
                                    <option :value="true">Available during build</option>
                                </select>
                            </label>
                            <label class="form-field">
                                Network hosts during build
                                <select
                                    v-model="advancedSettings.include_network_hosts_in_build"
                                    class="coolify-input"
                                    :disabled="!canEdit"
                                >
                                    <option :value="true">Add network containers as hosts</option>
                                    <option :value="false">Do not add (preserves cache)</option>
                                </select>
                            </label>
                        </div>

                        <div class="coolify-subsection">
                            <h3>Deployment</h3>
                            <div class="form-grid">
                                <label class="form-field">
                                    Auto deploy
                                    <select
                                        v-model="advancedSettings.is_auto_deploy_enabled"
                                        class="coolify-input"
                                        :disabled="!canEdit"
                                    >
                                        <option :value="true">Deploy on push (webhooks)</option>
                                        <option :value="false">Manual deployments only</option>
                                    </select>
                                </label>
                                <label class="form-field">
                                    Preview deployments
                                    <select
                                        v-model="advancedSettings.is_preview_deployments_enabled"
                                        class="coolify-input"
                                        :disabled="!canEdit"
                                    >
                                        <option :value="true">Enabled</option>
                                        <option :value="false">Disabled</option>
                                    </select>
                                </label>
                                <label class="form-field">
                                    Debug logging
                                    <select
                                        v-model="advancedSettings.is_debug_enabled"
                                        class="coolify-input"
                                        :disabled="!canEdit"
                                    >
                                        <option :value="false">Disabled</option>
                                        <option :value="true">Enabled</option>
                                    </select>
                                </label>
                            </div>
                        </div>

                        <div class="coolify-subsection">
                            <h3>Git</h3>
                            <div class="form-grid">
                                <label class="form-field">
                                    Submodules
                                    <select
                                        v-model="advancedSettings.is_git_submodules_enabled"
                                        class="coolify-input"
                                        :disabled="!canEdit"
                                    >
                                        <option :value="true">Clone submodules</option>
                                        <option :value="false">Ignore submodules</option>
                                    </select>
                                </label>
                                <label class="form-field">
                                    Git LFS
                                    <select
                                        v-model="advancedSettings.is_git_lfs_enabled"
                                        class="coolify-input"
                                        :disabled="!canEdit"
                                    >
                                        <option :value="true">Enabled</option>
                                        <option :value="false">Disabled</option>
                                    </select>
                                </label>
                                <label class="form-field">
                                    Clone depth
                                    <select
                                        v-model="advancedSettings.is_git_shallow_clone_enabled"
                                        class="coolify-input"
                                        :disabled="!canEdit"
                                    >
                                        <option :value="true">
                                            Shallow clone (latest commit only)
                                        </option>
                                        <option :value="false">Full clone (entire history)</option>
                                    </select>
                                </label>
                            </div>
                        </div>
                    </SettingsSection>

                    <SettingsSection
                        id="advanced-container-section"
                        title="Container"
                    >
                        <div class="form-grid">
                            <label class="form-field">
                                Consistent container name
                                <select
                                    v-model="advancedSettings.is_consistent_container_name_enabled"
                                    class="coolify-input"
                                    :disabled="!canEdit"
                                >
                                    <option :value="false">Disabled</option>
                                    <option :value="true">Enabled</option>
                                </select>
                            </label>
                            <label class="form-field">
                                Include timestamps in logs
                                <select
                                    v-model="advancedSettings.is_include_timestamps"
                                    class="coolify-input"
                                    :disabled="!canEdit"
                                >
                                    <option :value="false">Disabled</option>
                                    <option :value="true">Enabled</option>
                                </select>
                            </label>
                            <label class="form-field">
                                Connect to predefined network
                                <select
                                    v-model="advancedSettings.connect_to_docker_network"
                                    class="coolify-input"
                                    :disabled="!canEdit"
                                >
                                    <option :value="false">Disabled</option>
                                    <option :value="true">Enabled</option>
                                </select>
                            </label>
                            <label class="form-field">
                                GPU support
                                <select
                                    v-model="advancedSettings.is_gpu_enabled"
                                    class="coolify-input"
                                    :disabled="!canEdit"
                                >
                                    <option :value="false">Disabled</option>
                                    <option :value="true">Enabled</option>
                                </select>
                            </label>
                            <label class="form-field full-span">
                                Custom Docker Options
                                <input
                                    v-model="form.custom_docker_run_options"
                                    class="coolify-input"
                                    placeholder="--cap-add SYS_ADMIN"
                                    :readonly="!canEdit"
                                />
                            </label>
                            <label class="form-field full-span">
                                Custom Container Labels (KEY=VALUE per line)
                                <textarea
                                    v-model="form.custom_labels"
                                    class="coolify-input code-editor"
                                    rows="5"
                                    :readonly="!canEdit"
                                />
                            </label>
                        </div>
                    </SettingsSection>

                    <SettingsSection
                        id="advanced-proxy-section"
                        title="Proxy"
                    >
                        <div class="form-grid">
                            <label class="form-field">
                                Gzip compression
                                <select
                                    v-model="advancedSettings.is_gzip_enabled"
                                    class="coolify-input"
                                    :disabled="!canEdit"
                                >
                                    <option :value="true">Enabled</option>
                                    <option :value="false">Disabled</option>
                                </select>
                            </label>
                            <label class="form-field">
                                Path prefixes
                                <select
                                    v-model="advancedSettings.is_stripprefix_enabled"
                                    class="coolify-input"
                                    :disabled="!canEdit"
                                >
                                    <option :value="true">Strip prefixes</option>
                                    <option :value="false">Keep paths as-is</option>
                                </select>
                            </label>
                            <label class="form-field">
                                Force HTTPS
                                <select
                                    v-model="advancedSettings.is_force_https_enabled"
                                    class="coolify-input"
                                    :disabled="!canEdit"
                                >
                                    <option :value="true">Enabled</option>
                                    <option :value="false">Disabled</option>
                                </select>
                            </label>
                            <label class="form-field">
                                Single Page Application (SPA)
                                <select
                                    v-model="advancedSettings.is_spa"
                                    class="coolify-input"
                                    :disabled="!canEdit"
                                >
                                    <option :value="false">Disabled</option>
                                    <option :value="true">Enabled</option>
                                </select>
                            </label>
                        </div>
                    </SettingsSection>
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
                    v-else-if="section === 'metrics' || section === 'analytics'"
                    :title="section === 'analytics' ? 'Analytics' : 'Metrics'"
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
                    v-else-if="section === 'security'"
                    title="Security"
                >
                    <p class="page-muted">
                        Image vulnerability scanning and security policy controls are managed under
                        <a href="/vulnerabilities">Security &amp; Vulnerability Scans</a>.
                    </p>
                </SettingsSection>
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
            ref="domainDialog"
            class="workspace-dialog"
        >
            <form
                class="dialog-form"
                @submit.prevent="saveDomainModal"
            >
                <div class="dialog-heading">
                    <h2>{{ domainForm.index >= 0 ? 'Edit Domain' : 'Add Domain' }}</h2>
                    <button
                        type="button"
                        class="chrome-button"
                        aria-label="Close"
                        @click="domainDialog?.close()"
                    >
                        ✕
                    </button>
                </div>
                <label class="form-field"
                    >Domain *<input
                        v-model="domainForm.url"
                        class="coolify-input"
                        placeholder="https://app.example.com"
                        required
                /></label>
                <div class="form-grid">
                    <label class="form-field"
                        >Internal port<input
                            v-model="domainForm.port"
                            class="coolify-input"
                            placeholder="3000"
                    /></label>
                    <label class="form-field"
                        >Protocol redirect<select
                            v-model="domainForm.forceHttps"
                            class="coolify-input"
                        >
                            <option :value="true">Force HTTPS</option>
                            <option :value="false">Disabled</option>
                        </select></label
                    >
                    <label class="form-field full-span"
                        >Domain redirect<select
                            v-model="domainForm.redirect"
                            class="coolify-input"
                        >
                            <option value="both">No redirect</option>
                            <option value="www">Redirect to www</option>
                            <option value="non-www">Redirect to non-www</option>
                        </select></label
                    >
                </div>
                <div
                    class="dialog-actions"
                    style="justify-content: space-between"
                >
                    <button
                        type="button"
                        class="coolify-btn"
                        @click="generateDomainIntoModal"
                    >
                        Regenerate hostname
                    </button>
                    <div style="display: flex; gap: 8px">
                        <button
                            type="button"
                            class="coolify-btn"
                            @click="domainDialog?.close()"
                        >
                            Cancel
                        </button>
                        <button
                            type="submit"
                            class="coolify-btn coolify-btn-primary"
                            :disabled="busy"
                        >
                            Save
                        </button>
                    </div>
                </div>
            </form>
        </dialog>
        <dialog
            ref="envDialog"
            class="workspace-dialog wide-dialog"
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
                <div class="form-grid">
                    <label class="form-field"
                        >Name *<input
                            v-model="envKey"
                            class="coolify-input"
                            placeholder="NODE_ENV"
                            required
                    /></label>
                    <label class="form-field"
                        >Comment<input
                            v-model="envComment"
                            class="coolify-input"
                            placeholder="Optional note"
                    /></label>
                </div>
                <label class="form-field"
                    >Value *<textarea
                        v-if="envMultiline"
                        v-model="envValue"
                        class="coolify-input code-editor"
                        rows="4"
                        placeholder="production"
                    /><input
                        v-else
                        v-model="envValue"
                        class="coolify-input"
                        placeholder="production"
                /></label>
                <p
                    class="page-muted"
                    style="margin: -6px 0 0; font-size: 12px"
                >
                    Tip: Type <code>&#123;&#123;</code> to reference a shared environment variable
                </p>
                <div class="form-grid">
                    <label class="form-field"
                        >Value type<select
                            v-model="envMultiline"
                            class="coolify-input"
                        >
                            <option :value="false">Single line</option>
                            <option :value="true">Multiline</option>
                        </select></label
                    >
                    <label class="form-field"
                        >Interpolation<select
                            v-model="envLiteral"
                            class="coolify-input"
                        >
                            <option :value="false">Interpolate $VARIABLES</option>
                            <option :value="true">Literal (no interpolation)</option>
                        </select></label
                    >
                    <label class="form-field"
                        >Build time<select
                            v-model="envBuildtime"
                            class="coolify-input"
                        >
                            <option :value="true">Available during build</option>
                            <option :value="false">Not available during build</option>
                        </select></label
                    >
                    <label class="form-field"
                        >Runtime<select
                            v-model="envRuntime"
                            class="coolify-input"
                        >
                            <option :value="true">Available in the container</option>
                            <option :value="false">Not available in the container</option>
                        </select></label
                    >
                </div>
                <label class="check-field"
                    ><input
                        v-model="envPreview"
                        type="checkbox"
                    />Available for Preview deployments</label
                >
                <p
                    v-if="error"
                    role="alert"
                    class="page-error"
                >
                    {{ error }}
                </p>
                <div class="dialog-actions">
                    <button
                        type="submit"
                        class="coolify-btn coolify-btn-primary"
                        :disabled="busy"
                    >
                        Add Variable
                    </button>
                </div>
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
