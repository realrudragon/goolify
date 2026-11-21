<script setup lang="ts">
defineProps<{ name: string; size?: number }>();
</script>
<template>
    <span
        class="app-icon"
        aria-hidden="true"
        :style="{
            maskImage: `url('/icons/${name}.svg')`,
            width: `${size ?? 16}px`,
            height: `${size ?? 16}px`,
        }"
    ></span>
</template>
