<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import { navigation, type NavigationEntry } from '../lib/navigation';
import { session, loadSession } from '../lib/session';
import AppIcon from './AppIcon.vue';
defineProps<{ mobile?: boolean }>();
const submenu = ref('');
const pathname = ref('/');
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
onMounted(() => {
    pathname.value = window.location.pathname;
    const parent = navigation
        .flatMap((group) => group.entries)
        .find((item) => item.children?.some((child) => active(child.href)));
    submenu.value = parent?.id ?? '';
    void loadSession().catch(() => {});
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
        <template v-if="selectedMenu">
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
