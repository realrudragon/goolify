<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue';
import { session, loadSession } from '../lib/session';
import LogoutButton from '../features/auth/LogoutButton.vue';
import AppIcon from './AppIcon.vue';
const menu = ref<HTMLDetailsElement>();
const username = computed(() => session.me?.email.split('@')[0] || 'Account');
function dismiss(event: MouseEvent | KeyboardEvent) {
    if (!menu.value?.open) return;
    if (event instanceof KeyboardEvent && event.key === 'Escape') {
        menu.value.open = false;
        menu.value.querySelector('summary')?.focus();
    } else if (
        event instanceof MouseEvent &&
        event.target instanceof Node &&
        !menu.value.contains(event.target)
    )
        menu.value.open = false;
}
onMounted(() => {
    void loadSession().catch(() => {});
    window.addEventListener('click', dismiss);
    window.addEventListener('keydown', dismiss);
});
onUnmounted(() => {
    window.removeEventListener('click', dismiss);
    window.removeEventListener('keydown', dismiss);
});
function toggleTheme() {
    document.documentElement.classList.toggle('dark');
    try {
        localStorage.setItem(
            'theme',
            document.documentElement.classList.contains('dark') ? 'dark' : 'light',
        );
    } catch {
        /* Browser storage is optional. */
    }
}
function openHelp() {
    if (menu.value) menu.value.open = false;
    window.dispatchEvent(new Event('open-help-feedback'));
}
</script>
<template>
    <details
        ref="menu"
        class="account-menu"
    >
        <summary
            class="account-trigger"
            :title="session.me?.email"
        >
            <span class="account-avatar">{{ username[0].toUpperCase() }}</span
            ><span class="account-name">{{ username }}</span
            ><span aria-hidden="true">⌄</span>
        </summary>
        <div class="account-popover">
            <p class="account-email">{{ session.me?.email }}</p>
            <a href="/profile"><AppIcon name="teams" /> Profile</a>
            <button
                type="button"
                data-testid="theme-toggle"
                @click="toggleTheme"
            >
                <AppIcon name="layers" /> Toggle light / dark theme
            </button>
            <button
                type="button"
                @click="openHelp"
            >
                <AppIcon name="feedback" /> Help &amp; Feedback
            </button>
            <LogoutButton />
        </div>
    </details>
</template>
