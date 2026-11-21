<script setup lang="ts">
import { nextTick, onMounted, onUnmounted, ref, watch } from 'vue';
import { apiRequest } from '../lib/api';
import { usePageLifecycle } from '../lib/use-page-lifecycle';
import AppIcon from './AppIcon.vue';
const dialog = ref<HTMLDialogElement>();
const input = ref<HTMLInputElement>();
const query = ref('');
const results = ref<Array<{ type: string; uuid: string; name: string }>>([]);
const error = ref('');
const loading = ref(false);
let generation = 0;
let timer: ReturnType<typeof setTimeout> | undefined;
const lifecycle = usePageLifecycle();
async function search() {
    const current = ++generation;
    loading.value = true;
    error.value = '';
    try {
        const payload = await apiRequest<{ results: typeof results.value }>(
            `/api/v1/search?q=${encodeURIComponent(query.value)}`,
            { signal: lifecycle.signal },
        );
        if (generation === current) results.value = payload.results;
    } catch (err) {
        if (generation === current && (err as Error).name !== 'AbortError')
            error.value = (err as Error).message;
    } finally {
        if (generation === current) loading.value = false;
    }
}
async function open() {
    dialog.value?.showModal();
    await nextTick();
    input.value?.focus();
    void search();
}
function shortcut(event: KeyboardEvent) {
    if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'k') {
        event.preventDefault();
        void open();
    }
}
watch(query, () => {
    generation++;
    clearTimeout(timer);
    timer = setTimeout(() => {
        void search();
    }, 180);
});
lifecycle.onTeamSwitch(() => {
    generation++;
    results.value = [];
    dialog.value?.close();
});
onMounted(() => {
    window.addEventListener('open-global-search', open);
    window.addEventListener('keydown', shortcut);
});
onUnmounted(() => {
    generation++;
    clearTimeout(timer);
    window.removeEventListener('open-global-search', open);
    window.removeEventListener('keydown', shortcut);
});
</script>
<template>
    <dialog
        ref="dialog"
        class="search-dialog"
        aria-label="Search"
    >
        <div class="search-dialog-input">
            <AppIcon
                name="search"
                :size="20"
            /><label
                class="sr-only"
                for="global-search"
                >Search resources</label
            ><input
                id="global-search"
                ref="input"
                v-model="query"
                placeholder="Search projects and servers..."
                autocomplete="off"
            /><button
                type="button"
                class="chrome-button"
                aria-label="Close search"
                @click="dialog?.close()"
            >
                Esc
            </button>
        </div>
        <div
            class="search-results"
            aria-live="polite"
        >
            <p
                v-if="error"
                role="alert"
            >
                {{ error }}
            </p>
            <p v-else-if="loading">Searching...</p>
            <p v-else-if="!results.length">No matching resources.</p>
            <a
                v-for="result in results"
                :key="`${result.type}:${result.uuid}`"
                :href="`/${result.type}/${encodeURIComponent(result.uuid)}`"
                ><AppIcon :name="result.type === 'project' ? 'projects' : 'servers'" /><span>{{
                    result.name
                }}</span
                ><small>{{ result.type }}</small></a
            >
        </div>
    </dialog>
</template>
