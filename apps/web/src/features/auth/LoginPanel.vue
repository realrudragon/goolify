<script setup lang="ts">
import { onMounted, ref } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const lifecycle = usePageLifecycle();
const hydrated = ref(false);
const mode = ref<'login' | 'register' | 'forgot' | 'reset'>('login');
const name = ref('');
const email = ref('');
const password = ref('');
const passwordConfirm = ref('');
const totpCode = ref('');
const resetToken = ref('');
const error = ref('');
const notice = ref('');
const busy = ref(false);
const registrationEnabled = ref(false);
const passkeysEnabled = ref(false);
const oauthProviders = ref<string[]>([]);

function providerLabel(provider: string) {
  const labels: Record<string, string> = {
    github: 'GitHub',
    google: 'Google',
    gitlab: 'GitLab',
    bitbucket: 'Bitbucket',
    discord: 'Discord',
    azure: 'Microsoft Entra ID',
    entra: 'Microsoft Entra ID',
    authentik: 'Authentik',
    zitadel: 'Zitadel',
    clerk: 'Clerk',
  };
  return labels[provider.toLowerCase()] ?? provider;
}

function switchMode(next: 'login' | 'register' | 'forgot' | 'reset') {
  mode.value = next;
  error.value = '';
  notice.value = '';
}

onMounted(async () => {
  hydrated.value = true;
  const params = new URLSearchParams(window.location.search);
  const tok = params.get('reset_token');
  if (tok) {
    resetToken.value = tok;
    mode.value = 'reset';
  }
  try {
    const opts = await apiRequest<{
      is_registration_enabled?: boolean;
      passkeys_enabled?: boolean;
      oauth_providers?: string[];
    }>('/app/auth/options', { signal: lifecycle.signal });
    registrationEnabled.value = Boolean(opts.is_registration_enabled);
    passkeysEnabled.value =
      Boolean(opts.passkeys_enabled) && typeof window !== 'undefined' && 'PublicKeyCredential' in window;
    oauthProviders.value = opts.oauth_providers ?? [];
  } catch {
    // Keep defaults if options endpoint is unreachable
  }
});

async function signInWithPasskey() {
  if (busy.value) return;
  busy.value = true;
  error.value = '';
  try {
    const opts = await apiRequest<{ challenge: string; rpId: string; timeout: number }>(
      '/app/auth/passkey/options',
      { method: 'POST', body: '{}' },
    );
    const challengeBytes = new TextEncoder().encode(opts.challenge);
    const cred = (await navigator.credentials.get({
      publicKey: {
        challenge: challengeBytes,
        rpId: opts.rpId,
        timeout: opts.timeout,
        userVerification: 'preferred',
      },
    })) as PublicKeyCredential | null;
    if (!cred) {
      error.value = 'Passkey sign-in was cancelled.';
      return;
    }
    await apiRequest('/app/auth/passkey/login', {
      method: 'POST',
      body: JSON.stringify({ challenge: opts.challenge, credential_id: cred.id }),
    });
    window.location.assign('/');
  } catch (err) {
    error.value = (err as Error).message || 'Passkey sign-in failed.';
  } finally {
    busy.value = false;
  }
}

async function submit() {
  if (busy.value) return;
  busy.value = true;
  error.value = '';
  notice.value = '';
  try {
    if (mode.value === 'login') {
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
      return;
    }
    if (mode.value === 'register') {
      if (password.value !== passwordConfirm.value) {
        error.value = 'Passwords do not match.';
        return;
      }
      await apiRequest('/app/auth/register', {
        method: 'POST',
        signal: lifecycle.signal,
        body: JSON.stringify({
          name: name.value,
          email: email.value,
          password: password.value,
          password_confirmation: passwordConfirm.value,
        }),
      });
      window.location.assign('/');
      return;
    }
    if (mode.value === 'forgot') {
      const res = await apiRequest<{ message?: string }>('/forgot-password', {
        method: 'POST',
        signal: lifecycle.signal,
        body: JSON.stringify({ email: email.value }),
      });
      notice.value = res.message ?? 'If an account matches that email, a password reset link has been sent.';
      return;
    }
    if (mode.value === 'reset') {
      if (password.value !== passwordConfirm.value) {
        error.value = 'Passwords do not match.';
        return;
      }
      const res = await apiRequest<{ message?: string }>('/reset-password', {
        method: 'POST',
        signal: lifecycle.signal,
        body: JSON.stringify({
          token: resetToken.value,
          password: password.value,
          password_confirmation: passwordConfirm.value,
        }),
      });
      password.value = '';
      passwordConfirm.value = '';
      resetToken.value = '';
      mode.value = 'login';
      notice.value = res.message ?? 'Password has been reset. You can now sign in.';
    }
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
    @submit.prevent="submit"
  >
    <div class="auth-card-heading">
      <h1>
        {{
          mode === 'register'
            ? 'Create an account'
            : mode === 'forgot'
              ? 'Reset your password'
              : mode === 'reset'
                ? 'Choose a new password'
                : 'Sign in to Goolify'
        }}
      </h1>
      <p>
        {{
          mode === 'register'
            ? 'Register a new account on this instance.'
            : mode === 'forgot'
              ? 'Enter your email address to receive a password reset link.'
              : mode === 'reset'
                ? 'Enter a new password for your account.'
                : 'Sign in to manage your applications and infrastructure.'
        }}
      </p>
    </div>
    <div class="auth-card-body">
      <p v-if="error" role="alert" class="auth-alert">{{ error }}</p>
      <p v-if="notice" role="status" class="auth-notice">{{ notice }}</p>

      <div v-if="mode === 'register'" class="auth-field">
        <label for="register-name">Name</label>
        <input id="register-name" v-model="name" name="name" type="text" autocomplete="name" class="coolify-input" placeholder="Your name" required />
      </div>

      <div v-if="mode !== 'reset'" class="auth-field">
        <label for="login-email">Email</label>
        <input id="login-email" v-model="email" name="email" type="email" autocomplete="email" class="coolify-input" placeholder="Email" required autofocus />
      </div>

      <div v-if="mode === 'reset'" class="auth-field">
        <label for="reset-token">Reset token</label>
        <input id="reset-token" v-model="resetToken" name="token" type="text" class="coolify-input" placeholder="Reset token" required />
      </div>

      <div v-if="mode !== 'forgot'" class="auth-field">
        <label for="login-password">{{ mode === 'reset' ? 'New Password' : 'Password' }}</label>
        <input
          id="login-password"
          v-model="password"
          name="password"
          type="password"
          :autocomplete="mode === 'login' ? 'current-password' : 'new-password'"
          class="coolify-input"
          placeholder="Password"
          required
        />
      </div>

      <div v-if="mode === 'register' || mode === 'reset'" class="auth-field">
        <label for="confirm-password">Confirm Password</label>
        <input
          id="confirm-password"
          v-model="passwordConfirm"
          name="password_confirmation"
          type="password"
          autocomplete="new-password"
          class="coolify-input"
          placeholder="Confirm Password"
          required
        />
      </div>

      <div v-if="mode === 'login'" class="auth-field">
        <label for="login-totp">Two-factor authentication code <span class="auth-optional">(if enabled)</span></label>
        <input id="login-totp" v-model="totpCode" name="code" autocomplete="one-time-code" inputmode="numeric" class="coolify-input" placeholder="2FA Code (optional)" />
      </div>

      <button type="submit" class="coolify-btn coolify-btn-primary auth-submit" :disabled="busy">
        {{
          busy
            ? 'Please wait...'
            : mode === 'register'
              ? 'Register'
              : mode === 'forgot'
                ? 'Send reset link'
                : mode === 'reset'
                  ? 'Reset password'
                  : 'Login'
        }}
      </button>

      <div v-if="mode === 'login' && (oauthProviders.length || passkeysEnabled)" class="auth-oauth-list">
        <button
          v-if="passkeysEnabled"
          type="button"
          class="coolify-btn auth-submit"
          :disabled="busy"
          @click="signInWithPasskey"
        >
          Sign in with Passkey
        </button>
        <a
          v-for="prov in oauthProviders"
          :key="prov"
          :href="`/auth/${encodeURIComponent(prov)}/redirect`"
          class="coolify-btn auth-submit"
        >
          Continue with {{ providerLabel(prov) }}
        </a>
      </div>

      <div class="auth-links">
        <button v-if="mode === 'login'" type="button" class="auth-link-btn" @click="switchMode('forgot')">
          Forgot password?
        </button>
        <button v-if="mode === 'login' && registrationEnabled" type="button" class="auth-link-btn" @click="switchMode('register')">
          Create an account
        </button>
        <button v-if="mode !== 'login'" type="button" class="auth-link-btn" @click="switchMode('login')">
          Back to sign in
        </button>
      </div>
    </div>
  </form>
</template>
