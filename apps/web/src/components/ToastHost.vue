<script setup lang="ts">
import { dismissToast, toastState } from '../lib/toast';
</script>

<template>
    <div
        class="toast-host"
        aria-live="polite"
        aria-atomic="false"
    >
        <div
            v-for="item in toastState.items"
            :key="item.id"
            class="toast-item"
            :class="`toast-${item.kind}`"
            role="status"
        >
            <span>{{ item.message }}</span>
            <button
                type="button"
                class="toast-close"
                aria-label="Dismiss notification"
                @click="dismissToast(item.id)"
            >
                ✕
            </button>
        </div>
    </div>
</template>
