<script setup lang="ts">
import { onMounted, ref } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const lifecycle = usePageLifecycle();
const hydrated = ref(false);
const email = ref('');
const password = ref('');
const totpCode = ref('');
const error = ref('');

onMounted(() => {
  hydrated.value = true;
});

async function login() {
  error.value = '';
  try {
    await apiRequest('/app/auth/login', {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({
        email: email.value,
        password: password.value,
        code: totpCode.value || undefined,
      }),
    });
    window.location.assign('/');
  } catch (err) {
    error.value = (err as Error).message || 'Authentication failed';
  }
}
</script>

<template>
  <form
    class="coolify-card"
    style="max-width:380px;margin:60px auto;"
    :data-hydrated="hydrated ? 'true' : 'false'"
    @submit.prevent="login"
  >
    <div class="coolify-well" style="display:grid;gap:10px;">
      <strong>Sign in to Goolify</strong>
      <input v-model="email" type="email" class="coolify-input" placeholder="Email" required />
      <input v-model="password" type="password" class="coolify-input" placeholder="Password" required />
      <input v-model="totpCode" class="coolify-input" placeholder="2FA Code (optional)" />
      <button type="submit" class="coolify-btn coolify-btn-primary">Login</button>
      <p v-if="error" role="alert">{{ error }}</p>
    </div>
  </form>
</template>
