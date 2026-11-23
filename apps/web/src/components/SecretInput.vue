<script setup lang="ts">
import { ref } from 'vue';
import { pushToast } from '../lib/toast';

const props = defineProps<{
    value: string;
    label?: string;
}>();

const revealed = ref(false);

async function copyValue() {
    try {
        await navigator.clipboard.writeText(props.value ?? '');
        pushToast(`${props.label ?? 'Value'} copied to clipboard.`, 'success');
    } catch {
        pushToast('Unable to copy to clipboard.', 'error');
    }
}
</script>

<template>
    <div class="secret-input-row">
        <input
            :type="revealed ? 'text' : 'password'"
            :value="value"
            readonly
            :aria-label="label ?? 'Secret value'"
            class="coolify-input"
        />
        <button
            type="button"
            class="coolify-btn"
            @click="revealed = !revealed"
        >
            {{ revealed ? 'Hide' : 'Show' }}
        </button>
        <button
            type="button"
            class="coolify-btn"
            @click="copyValue"
        >
            Copy
        </button>
    </div>
</template>
