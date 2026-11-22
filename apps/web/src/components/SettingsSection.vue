<script setup lang="ts">
defineProps<{ title: string; helper?: string }>();
</script>

<template>
    <section
        class="application-settings-section"
        :data-settings-section-title="title"
    >
        <header>
            <h3
                :title="helper"
                :class="{ 'has-helper': helper }"
            >
                {{ title }}
            </h3>
            <div class="page-actions"><slot name="actions" /></div>
        </header>
        <div
            v-if="$slots.default"
            class="application-settings-section-body"
        >
            <slot />
        </div>
    </section>
</template>
