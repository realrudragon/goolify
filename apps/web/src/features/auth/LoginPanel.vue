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
const busy = ref(false);

onMounted(() => {
  hydrated.value = true;
});

async function login() {
  if (busy.value) return;
  busy.value = true;
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
  } finally {
    busy.value = false;
  }
}
</script>

<template>
  <form
    class="auth-card"
    :data-hydrated="hydrated ? 'true' : 'false'"
    @submit.prevent="login"
  >
    <div class="auth-card-heading">
      <h1>Sign in to Goolify</h1>
      <p>Sign in to manage your applications and infrastructure.</p>
    </div>
    <div class="auth-card-body">
      <p v-if="error" role="alert" class="auth-alert">{{ error }}</p>
      <div class="auth-field">
        <label for="login-email">Email</label>
        <input id="login-email" v-model="email" name="email" type="email" autocomplete="email" class="coolify-input" placeholder="Email" required autofocus />
      </div>
      <div class="auth-field">
        <label for="login-password">Password</label>
        <input id="login-password" v-model="password" name="password" type="password" autocomplete="current-password" class="coolify-input" placeholder="Password" required />
      </div>
      <div class="auth-field">
        <label for="login-totp">Two-factor authentication code <span class="auth-optional">(if enabled)</span></label>
        <input id="login-totp" v-model="totpCode" name="code" autocomplete="one-time-code" inputmode="numeric" class="coolify-input" placeholder="2FA Code (optional)" />
      </div>
      <button type="submit" class="coolify-btn coolify-btn-primary auth-submit" :disabled="busy">{{ busy ? 'Signing in...' : 'Login' }}</button>
    </div>
  </form>
</template>
