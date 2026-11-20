import { defineConfig } from 'astro/config';
import vue from '@astrojs/vue';

export default defineConfig({
  output: 'static',
  integrations: [vue()],
  vite: {
    css: {
      // Isolate standalone Astro build from legacy root postcss.config.cjs
      postcss: {
        plugins: [],
      },
    },
  },
});
