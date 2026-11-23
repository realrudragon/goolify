<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue';
import { navigation, type NavigationEntry } from '../lib/navigation';
import { session, loadSession } from '../lib/session';
import AppIcon from './AppIcon.vue';

defineProps<{ mobile?: boolean }>();
const submenu = ref('');
const pathname = ref(typeof window !== 'undefined' ? window.location.pathname : '/');
const activeAnchor = ref('advanced-build-section');

const role = computed(() =>
    session.me?.teams.find((team) => team.id === session.me?.session_team_id)?.role?.toLowerCase(),
);
const admin = computed(() => role.value === 'admin' || role.value === 'owner');
const visible = (item: NavigationEntry) =>
    (!item.instanceOnly || session.me?.is_instance_admin) && (!item.adminOnly || admin.value);
const groups = computed(() =>
    navigation.map((group) => ({ ...group, entries: group.entries.filter(visible) })),
);
const selectedMenu = computed(() =>
    groups.value.flatMap((group) => group.entries).find((item) => item.id === submenu.value),
);
const menuItems = computed(() => selectedMenu.value?.children?.filter(visible) ?? []);

const resourceContext = computed(() => {
    const m = pathname.value.match(
        /^\/project\/([^/]+)\/environment\/([^/]+)\/(application|service|database)\/([^/]+)(?:\/(.*))?$/,
    );
    if (!m) return null;
    const rawSub = (m[5] || 'general').split('/')[0] || 'general';
    return {
        projectUuid: m[1],
        environmentUuid: m[2],
        kind: m[3] as 'application' | 'service' | 'database',
        uuid: m[4],
        subpage: rawSub,
        base: `/project/${m[1]}/environment/${m[2]}/${m[3]}/${m[4]}`,
    };
});

const resourceGroups = computed(() => {
    const ctx = resourceContext.value;
    if (!ctx) return [];
    const b = ctx.base;
    if (ctx.kind === 'application') {
        return [
            [
                { label: 'General', key: 'general', href: b, icon: 'settings' },
                { label: 'Domains', key: 'domains', href: `${b}/domains`, icon: 'network' },
                {
                    label: 'Environment Variables',
                    key: 'environment-variables',
                    href: `${b}/environment-variables`,
                    icon: 'variables',
                },
                {
                    label: 'Persistent Storage',
                    key: 'storage',
                    href: `${b}/storage`,
                    icon: 'storages',
                },
                {
                    label: 'Advanced',
                    key: 'advanced',
                    href: `${b}/advanced`,
                    icon: 'layers',
                    subAnchors: [
                        { id: 'advanced-build-section', label: 'Build & deploy' },
                        { id: 'advanced-container-section', label: 'Container' },
                        { id: 'advanced-proxy-section', label: 'Proxy' },
                    ],
                },
                {
                    label: 'Healthcheck',
                    key: 'healthcheck',
                    href: `${b}/healthcheck`,
                    icon: 'graph',
                },
            ],
            [
                { label: 'Runtime Logs', key: 'logs', href: `${b}/logs`, icon: 'file-content' },
                {
                    label: 'Deployment Logs',
                    key: 'deployment',
                    href: `${b}/deployment`,
                    icon: 'time-back',
                },
                {
                    label: 'Terminal',
                    key: 'terminal',
                    href: `${b}/terminal`,
                    icon: 'browser-terminal',
                },
                {
                    label: 'Files',
                    key: 'files',
                    href: `${b}/storage`,
                    icon: 'file-content',
                    experimental: true,
                },
                { label: 'Metrics', key: 'metrics', href: `${b}/metrics`, icon: 'graph' },
                { label: 'Analytics', key: 'analytics', href: `${b}/analytics`, icon: 'analytics' },
                {
                    label: 'Security',
                    key: 'security',
                    href: `${b}/security`,
                    icon: 'shield-alert',
                },
            ],
            [
                { label: 'Source', key: 'source', href: `${b}/source`, icon: 'sources' },
                { label: 'Servers', key: 'servers', href: `${b}/servers`, icon: 'servers' },
                {
                    label: 'Scheduled Tasks',
                    key: 'scheduled-tasks',
                    href: `${b}/scheduled-tasks`,
                    icon: 'time-back',
                },
                { label: 'Previews', key: 'previews', href: `${b}/previews`, icon: 'projects' },
                { label: 'Rollback', key: 'rollback', href: `${b}/rollback`, icon: 'time-back' },
                {
                    label: 'Resource Limits',
                    key: 'resource-limits',
                    href: `${b}/resource-limits`,
                    icon: 'settings',
                },
                { label: 'Tags', key: 'tags', href: `${b}/tags`, icon: 'tags' },
                { label: 'Danger Zone', key: 'danger', href: `${b}/danger`, icon: 'shield-alert' },
            ],
        ];
    }
    if (ctx.kind === 'service') {
        return [
            [
                { label: 'General', key: 'general', href: b, icon: 'settings' },
                { label: 'Domains', key: 'domains', href: `${b}/domains`, icon: 'network' },
                {
                    label: 'Environment Variables',
                    key: 'environment-variables',
                    href: `${b}/environment-variables`,
                    icon: 'variables',
                },
                {
                    label: 'Persistent Storage',
                    key: 'storage',
                    href: `${b}/storage`,
                    icon: 'storages',
                },
                {
                    label: 'Scheduled Tasks',
                    key: 'scheduled-tasks',
                    href: `${b}/scheduled-tasks`,
                    icon: 'time-back',
                },
            ],
            [
                { label: 'Runtime Logs', key: 'logs', href: `${b}/logs`, icon: 'file-content' },
                {
                    label: 'Terminal',
                    key: 'terminal',
                    href: `${b}/terminal`,
                    icon: 'browser-terminal',
                },
                { label: 'Metrics', key: 'metrics', href: `${b}/metrics`, icon: 'graph' },
                { label: 'Tags', key: 'tags', href: `${b}/tags`, icon: 'tags' },
                { label: 'Danger Zone', key: 'danger', href: `${b}/danger`, icon: 'shield-alert' },
            ],
        ];
    }
    return [
        [
            { label: 'General', key: 'general', href: b, icon: 'settings' },
            { label: 'Backups', key: 'backups', href: `${b}/backups`, icon: 'storages' },
            {
                label: 'Environment Variables',
                key: 'environment-variables',
                href: `${b}/environment-variables`,
                icon: 'variables',
            },
            { label: 'Persistent Storage', key: 'storage', href: `${b}/storage`, icon: 'storages' },
        ],
        [
            { label: 'Runtime Logs', key: 'logs', href: `${b}/logs`, icon: 'file-content' },
            { label: 'Terminal', key: 'terminal', href: `${b}/terminal`, icon: 'browser-terminal' },
            { label: 'Metrics', key: 'metrics', href: `${b}/metrics`, icon: 'graph' },
            { label: 'Tags', key: 'tags', href: `${b}/tags`, icon: 'tags' },
            { label: 'Danger Zone', key: 'danger', href: `${b}/danger`, icon: 'shield-alert' },
        ],
    ];
});

function scrollToAnchor(id: string) {
    activeAnchor.value = id;
    const el = document.getElementById(id);
    if (el) el.scrollIntoView({ behavior: 'smooth', block: 'start' });
}

function active(href?: string): boolean {
    if (!href) return false;
    let path = pathname.value;
    if (path.startsWith('/server/')) path = '/servers';
    if (path.startsWith('/project/')) path = '/projects';
    if (path.startsWith('/source/')) path = '/sources';
    return (
        href === path ||
        (href !== '/' &&
            path.startsWith(href + '/') &&
            !menuItems.value.some((item) => item.href !== href && item.href === path))
    );
}

function syncPathname() {
    pathname.value = window.location.pathname;
    if (!resourceContext.value) {
        const parent = navigation
            .flatMap((group) => group.entries)
            .find((item) => item.children?.some((child) => active(child.href)));
        submenu.value = parent?.id ?? '';
    }
}

onMounted(() => {
    syncPathname();
    window.addEventListener('popstate', syncPathname);
    window.addEventListener('coolify:navigate', syncPathname);
    void loadSession().catch(() => {});
});

onBeforeUnmount(() => {
    window.removeEventListener('popstate', syncPathname);
    window.removeEventListener('coolify:navigate', syncPathname);
});

function openSearch() {
    window.dispatchEvent(new Event('open-global-search'));
}
</script>
<template>
    <nav
        :aria-label="mobile ? 'Mobile navigation' : 'Primary navigation'"
        class="sidebar-navigation"
    >
        <button
            type="button"
            class="sidebar-search"
            aria-label="Search resources"
            @click="openSearch"
        >
            <AppIcon
                name="search"
                :size="18"
            /><span class="app-nav-label">Search</span><kbd>Ctrl+K</kbd>
        </button>

        <template v-if="resourceContext">
            <a
                href="/"
                class="nav-back"
                title="Back to Dashboard"
            >
                <AppIcon name="arrow-right" /> <span class="app-nav-label">Dashboard</span>
            </a>
            <template
                v-for="(grp, gIdx) in resourceGroups"
                :key="gIdx"
            >
                <div
                    v-if="gIdx > 0"
                    class="sidebar-divider"
                    aria-hidden="true"
                ></div>
                <div class="app-nav-group">
                    <template
                        v-for="item in grp"
                        :key="item.key + item.label"
                    >
                        <a
                            :href="item.href"
                            :title="item.label"
                            class="coolify-nav-link"
                            :aria-current="
                                resourceContext.subpage === item.key ? 'page' : undefined
                            "
                        >
                            <span class="app-nav-glyph"><AppIcon :name="item.icon" /></span>
                            <span class="app-nav-label">{{ item.label }}</span>
                        </a>
                        <div
                            v-if="
                                'subAnchors' in item &&
                                item.subAnchors &&
                                resourceContext.subpage === item.key
                            "
                            class="sidebar-subanchors"
                        >
                            <button
                                v-for="sub in item.subAnchors"
                                :key="sub.id"
                                type="button"
                                class="sidebar-subanchor-btn"
                                :class="{ active: activeAnchor === sub.id }"
                                @click="scrollToAnchor(sub.id)"
                            >
                                {{ sub.label }}
                            </button>
                        </div>
                    </template>
                </div>
            </template>
        </template>

        <template v-else-if="selectedMenu">
            <button
                type="button"
                class="nav-back"
                @click="submenu = ''"
            >
                <AppIcon name="arrow-right" /> Back
            </button>
            <p class="app-nav-heading">{{ selectedMenu.label }}</p>
            <a
                v-for="item in menuItems"
                :key="item.href"
                :href="item.href"
                :title="item.label"
                class="coolify-nav-link"
                :aria-current="active(item.href) ? 'page' : undefined"
                ><span class="app-nav-glyph"><AppIcon :name="item.icon" /></span
                ><span class="app-nav-label">{{ item.label }}</span></a
            >
        </template>

        <template v-else>
            <div
                v-for="(group, index) in groups"
                :key="index"
                class="app-nav-group"
            >
                <p
                    v-if="group.label"
                    class="app-nav-heading"
                >
                    {{ group.label }}
                </p>
                <template
                    v-for="item in group.entries"
                    :key="item.label"
                >
                    <a
                        v-if="item.href"
                        :href="item.href"
                        :title="item.label"
                        class="coolify-nav-link"
                        :aria-current="active(item.href) ? 'page' : undefined"
                        ><span class="app-nav-glyph"><AppIcon :name="item.icon" /></span
                        ><span class="app-nav-label">{{ item.label }}</span></a
                    >
                    <button
                        v-else
                        type="button"
                        :title="item.label"
                        class="coolify-nav-link"
                        @click="submenu = item.id ?? ''"
                    >
                        <span class="app-nav-glyph"><AppIcon :name="item.icon" /></span
                        ><span class="app-nav-label">{{ item.label }}</span>
                    </button>
                </template>
            </div>
        </template>
    </nav>
</template>
