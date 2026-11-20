<script setup lang="ts">
import { ref } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const lifecycle = usePageLifecycle();
const busy = ref(false);

async function logout() {
  if (busy.value) return;
  busy.value = true;
  try {
    await apiRequest('/app/auth/logout', {
      method: 'POST',
      signal: lifecycle.signal,
    });
  } catch {
    // Even if session was already expired, proceed to login screen
  } finally {
    window.location.assign('/login');
  }
}
</script>

<template>
  <button
    type="button"
    class="coolify-btn"
    style="width:100%;justify-content:center;margin-top:8px;"
    :disabled="busy"
    @click="logout"
  >
    {{ busy ? 'Signing out...' : 'Sign out' }}
  </button>
</template>
