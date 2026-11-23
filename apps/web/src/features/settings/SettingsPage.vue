<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import PageHeading from '../../components/PageHeading.vue';
import SettingsSection from '../../components/SettingsSection.vue';
import AppIcon from '../../components/AppIcon.vue';
import { apiRequest } from '../../lib/api';
import { session } from '../../lib/session';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const props = defineProps<{ page: string; subpage: string }>();
const lifecycle = usePageLifecycle();
const form = ref<Record<string, any>>({});
const list = ref<Record<string, any>[]>([]);
const search = ref('');
const error = ref('');
const status = ref('');
const busy = ref(false);
const dialog = ref<HTMLDialogElement>();
const name = ref('');
const value = ref('');
const description = ref('');
const invitationEmail = ref('');
const invitationRole = ref('member');
const twoFactor = ref<{ secret: string; otpauth_url: string; recovery_codes: string[] }>();
const code = ref('');
const profileTheme = ref('system');
const title = computed(
    () =>
        ({
            settings: 'Settings',
            team: 'Team',
            tags: 'Tags',
            'shared-variables': 'Shared Variables',
            profile: 'Profile',
        })[props.page] ?? props.page,
);
const canEdit = computed(() =>
    ['admin', 'owner'].includes(
        session.me?.teams.find((t) => t.id === session.me?.session_team_id)?.role?.toLowerCase() ??
            '',
    ),
);
const filtered = computed(() =>
    list.value.filter((item) =>
        Object.values(item).some(
            (v) =>
                typeof v === 'string' &&
                v.toLowerCase().includes(search.value.trim().toLowerCase()),
        ),
    ),
);
const sharedScope = computed(() => props.subpage.split('/')[0] || 'overview');
const sharedId = computed(() => props.subpage.split('/')[1]);
const sharedEndpoint = computed(() =>
    sharedScope.value === 'team'
        ? '/api/v1/team/envs'
        : sharedScope.value === 'project' && sharedId.value
          ? `/api/v1/projects/${sharedId.value}/envs`
          : sharedScope.value === 'server' && sharedId.value
            ? `/api/v1/servers/${sharedId.value}/envs`
            : '',
);
const oauthProvider = computed(() =>
    props.subpage.startsWith('oauth/') ? props.subpage.split('/')[1] : '',
);
const configFields = computed(() =>
    props.subpage.startsWith('oauth')
        ? [
              { key: 'enabled', label: 'Enabled', type: 'checkbox' },
              { key: 'client_id', label: 'Client ID' },
              { key: 'client_secret', label: 'Client Secret', type: 'password' },
              { key: 'redirect_uri', label: 'Redirect URI' },
              { key: 'tenant', label: 'Tenant' },
              { key: 'allow_registration', label: 'Allow registration', type: 'checkbox' },
          ]
        : props.subpage === 'advanced'
          ? [
                { key: 'is_registration_enabled', label: 'Registration enabled', type: 'checkbox' },
                { key: 'is_api_enabled', label: 'API access enabled', type: 'checkbox' },
                { key: 'is_mcp_server_enabled', label: 'MCP server enabled', type: 'checkbox' },
            ]
          : props.subpage === 'updates'
            ? [
                  { key: 'is_auto_update_enabled', label: 'Auto-update enabled', type: 'checkbox' },
                  { key: 'auto_update_frequency', label: 'Auto-update schedule (cron)' },
                  { key: 'update_check_frequency', label: 'Update check schedule (cron)' },
              ]
            : props.subpage === 'email'
              ? [
                    { key: 'smtp_enabled', label: 'Enable SMTP', type: 'checkbox' },
                    { key: 'smtp_host', label: 'SMTP Host' },
                    { key: 'smtp_port', label: 'SMTP Port', type: 'number' },
                    { key: 'smtp_username', label: 'SMTP Username' },
                    { key: 'smtp_password', label: 'SMTP Password', type: 'password' },
                    { key: 'smtp_from_address', label: 'SMTP From Address' },
                    { key: 'smtp_from_name', label: 'SMTP From Name' },
                    { key: 'resend_enabled', label: 'Enable Resend', type: 'checkbox' },
                    { key: 'resend_api_key', label: 'Resend API Key', type: 'password' },
                    { key: 'resend_from_address', label: 'Resend From Address' },
                    { key: 'resend_from_name', label: 'Resend From Name' },
                ]
              : props.subpage === 'image-scanning'
                ? [
                      { key: 'enabled', label: 'Enable image vulnerability scanning', type: 'checkbox' },
                      { key: 'block_on_critical', label: 'Block deploy on Critical CVEs', type: 'checkbox' },
                      { key: 'block_on_high', label: 'Block deploy on High CVEs', type: 'checkbox' },
                  ]
                : [
                      { key: 'instance_name', label: 'Instance name' },
                      { key: 'fqdn', label: 'Instance Domain (FQDN)' },
                      { key: 'instance_timezone', label: 'Instance Timezone' },
                      { key: 'allowed_ips', label: 'Allowed IPs (comma-separated)' },
                  ],
);
async function load() {
    error.value = '';
    try {
        if (props.page === 'settings') {
            if (props.subpage.startsWith('oauth')) {
                list.value = await apiRequest('/app/settings/oauth', { signal: lifecycle.signal });
                form.value = {
                    ...(list.value.find((p) => p.provider === oauthProvider.value) ?? {}),
                    client_secret: '',
                };
            } else if (['general', 'advanced', 'updates', 'email'].includes(props.subpage)) {
                const loaded = await apiRequest<Record<string, any>>('/api/v1/settings', {
                    signal: lifecycle.signal,
                });
                form.value = { ...loaded, smtp_password: '', resend_api_key: '' };
            } else if (props.subpage === 'image-scanning') {
                form.value = await apiRequest('/api/v1/security/image-scan-policy', {
                    signal: lifecycle.signal,
                });
            }
        } else if (props.page === 'team') {
            form.value = await apiRequest('/api/v1/team', { signal: lifecycle.signal });
            if (props.subpage === 'members')
                list.value = await apiRequest('/api/v1/team/members', { signal: lifecycle.signal });
            if (props.subpage === 'audit-log')
                list.value = await apiRequest('/api/v1/audit-events', { signal: lifecycle.signal });
        } else if (props.page === 'tags')
            list.value = await apiRequest('/api/v1/tags', { signal: lifecycle.signal });
        else if (props.page === 'shared-variables') {
            if (sharedEndpoint.value)
                list.value = await apiRequest(sharedEndpoint.value, { signal: lifecycle.signal });
            else if (['projects', 'project'].includes(sharedScope.value))
                list.value = await apiRequest('/api/v1/projects', { signal: lifecycle.signal });
            else if (['servers', 'server'].includes(sharedScope.value))
                list.value = await apiRequest('/api/v1/servers', { signal: lifecycle.signal });
        }
    } catch (err) {
        error.value = (err as Error).message;
    }
}
async function save() {
    busy.value = true;
    error.value = '';
    try {
        const endpoint =
            props.page === 'team'
                ? '/api/v1/team'
                : oauthProvider.value
                  ? `/app/settings/oauth/${oauthProvider.value}`
                  : props.subpage === 'image-scanning'
                    ? '/api/v1/security/image-scan-policy'
                    : '/api/v1/settings';
        const secretKeys = new Set(['client_secret', 'smtp_password', 'resend_api_key']);
        const keys =
            props.page === 'team' ? ['name', 'description'] : configFields.value.map((f) => f.key);
        const body = Object.fromEntries(
            keys
                .filter((key) => !secretKeys.has(key) || form.value[key])
                .map((key) => [
                    key,
                    key === 'smtp_port' ? Number(form.value[key]) || 587 : form.value[key],
                ]),
        );
        await apiRequest(endpoint, { method: 'PATCH', body: JSON.stringify(body) });
        await load();
        status.value = 'Saved.';
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        busy.value = false;
    }
}
async function create() {
    busy.value = true;
    error.value = '';
    try {
        if (props.page === 'team')
            await apiRequest(`/api/v1/teams/${session.me?.session_team_id}/invitations`, {
                method: 'POST',
                body: JSON.stringify({ email: invitationEmail.value, role: invitationRole.value }),
            });
        else
            await apiRequest(props.page === 'tags' ? '/api/v1/tags' : sharedEndpoint.value, {
                method: 'POST',
                body: JSON.stringify(
                    props.page === 'tags'
                        ? { name: name.value }
                        : { key: name.value, value: value.value },
                ),
            });
        dialog.value?.close();
        name.value = '';
        value.value = '';
        invitationEmail.value = '';
        status.value = 'Saved.';
        await load();
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        busy.value = false;
    }
}
async function setup2FA() {
    try {
        twoFactor.value = await apiRequest('/app/auth/2fa/enable', { method: 'POST' });
    } catch (err) {
        error.value = (err as Error).message;
    }
}
async function confirm2FA() {
    try {
        await apiRequest('/app/auth/2fa/confirm', {
            method: 'POST',
            body: JSON.stringify({ code: code.value }),
        });
        code.value = '';
        status.value = 'Two-factor authentication enabled. Save your recovery codes.';
    } catch (err) {
        error.value = (err as Error).message;
    }
}
function changeTheme() {
    localStorage.setItem('theme', profileTheme.value);
    document.documentElement.classList.toggle(
        'dark',
        profileTheme.value === 'dark' ||
            (profileTheme.value === 'system' && matchMedia('(prefers-color-scheme: dark)').matches),
    );
}
onMounted(() => {
    profileTheme.value = localStorage.getItem('theme') ?? 'system';
    void load();
});
</script>

<template>
    <section
        class="original-page"
        :data-testid="`${page}-page`"
    >
        <PageHeading :title="title"
            ><button
                v-if="
                    canEdit &&
                    (page === 'tags' ||
                        sharedEndpoint ||
                        (page === 'team' && subpage === 'members'))
                "
                type="button"
                class="coolify-btn coolify-btn-primary"
                @click="dialog?.showModal()"
            >
                <AppIcon name="plus" />{{
                    page === 'team' ? 'Invite member' : page === 'tags' ? 'New tag' : 'Add variable'
                }}
            </button></PageHeading
        >
        <p
            v-if="error"
            role="alert"
            class="page-error"
        >
            {{ error }}
        </p>
        <p
            v-if="status"
            role="status"
        >
            {{ status }}
        </p>
        <template v-if="page === 'settings'">
            <div
                v-if="subpage.startsWith('oauth')"
                class="settings-workspace"
            >
                <nav
                    class="settings-sidebar"
                    aria-label="Authentication providers"
                >
                    <a
                        v-for="provider in list"
                        :key="provider.provider"
                        :href="`/settings/oauth/${provider.provider}`"
                        :aria-current="oauthProvider === provider.provider ? 'page' : undefined"
                        >{{ provider.provider }}</a
                    >
                </nav>
                <form
                    v-if="oauthProvider"
                    @submit.prevent="save"
                >
                    <SettingsSection :title="oauthProvider"
                        ><template #actions
                            ><button
                                type="submit"
                                class="coolify-btn coolify-btn-primary"
                                :disabled="busy"
                            >
                                Save
                            </button></template
                        >
                        <div class="form-grid">
                            <label
                                v-for="field in configFields"
                                :key="field.key"
                                :class="
                                    field.type === 'checkbox'
                                        ? 'check-field full-span'
                                        : 'form-field'
                                "
                                ><input
                                    v-if="field.type === 'checkbox'"
                                    v-model="form[field.key]"
                                    type="checkbox" />{{ field.label
                                }}<input
                                    v-if="field.type !== 'checkbox'"
                                    v-model="form[field.key]"
                                    :type="field.type ?? 'text'"
                                    class="coolify-input"
                            /></label></div
                    ></SettingsSection>
                </form>
                <p
                    v-else
                    class="page-muted"
                >
                    Select an authentication provider.
                </p>
            </div>
            <form
                v-else-if="['general', 'advanced', 'updates', 'email', 'image-scanning'].includes(subpage)"
                @submit.prevent="save"
            >
                <SettingsSection
                    :title="
                        subpage === 'general'
                            ? 'General'
                            : subpage === 'advanced'
                              ? 'Access'
                              : subpage === 'updates'
                                ? 'Updates'
                                : subpage === 'email'
                                  ? 'Instance Email'
                                  : 'Image Scanning Policy'
                    "
                    ><template #actions
                        ><button
                            type="submit"
                            class="coolify-btn coolify-btn-primary"
                            :disabled="busy"
                        >
                            Save
                        </button></template
                    >
                    <div class="form-grid">
                        <label
                            v-for="field in configFields"
                            :key="field.key"
                            :class="
                                field.type === 'checkbox' ? 'check-field full-span' : 'form-field'
                            "
                            ><input
                                v-if="field.type === 'checkbox'"
                                v-model="form[field.key]"
                                type="checkbox" />{{ field.label
                            }}<input
                                v-if="field.type !== 'checkbox'"
                                v-model="form[field.key]"
                                :type="field.type ?? 'text'"
                                :placeholder="
                                    field.type === 'password'
                                        ? 'Leave empty to keep current value'
                                        : undefined
                                "
                                class="coolify-input"
                        /></label></div
                ></SettingsSection>
            </form>
            <SettingsSection
                v-else
                :title="subpage"
                ><p
                    class="page-error"
                    role="alert"
                >
                    This settings page has not been restored yet.
                </p></SettingsSection
            >
        </template>
        <template v-else-if="page === 'team' && subpage === 'general'"
            ><form @submit.prevent="save">
                <SettingsSection title="Team details"
                    ><template #actions
                        ><button
                            v-if="canEdit"
                            class="coolify-btn coolify-btn-primary"
                            :disabled="busy"
                        >
                            Save
                        </button></template
                    >
                    <div class="form-grid">
                        <label class="form-field"
                            >Name<input
                                v-model="form.name"
                                class="coolify-input"
                                required
                                :readonly="!canEdit" /></label
                        ><label class="form-field"
                            >Description<input
                                v-model="form.description"
                                class="coolify-input"
                                :readonly="!canEdit"
                        /></label></div
                ></SettingsSection></form
        ></template>
        <template v-else-if="page === 'profile'"
            ><SettingsSection
                v-if="subpage === 'appearance'"
                title="Appearance"
                ><label class="form-field"
                    >Theme<select
                        v-model="profileTheme"
                        class="coolify-input"
                        @change="changeTheme"
                    >
                        <option value="system">System</option>
                        <option value="light">Light</option>
                        <option value="dark">Dark</option>
                    </select></label
                ></SettingsSection
            >
            <div
                v-else
                class="settings-content"
            >
                <SettingsSection title="Profile details"
                    ><label class="form-field"
                        >Email<input
                            :value="session.me?.email"
                            class="coolify-input"
                            readonly /></label></SettingsSection
                ><SettingsSection title="Two-factor authentication"
                    ><template #actions
                        ><button
                            type="button"
                            class="coolify-btn"
                            @click="setup2FA"
                        >
                            Enable
                        </button></template
                    >
                    <p class="page-muted">
                        Add additional security to your account using two-factor authentication.
                    </p>
                    <form
                        v-if="twoFactor"
                        class="settings-content"
                        @submit.prevent="confirm2FA"
                    >
                        <label class="form-field"
                            >Setup key<input
                                :value="twoFactor.secret"
                                readonly
                                class="coolify-input code-text" /></label
                        ><a :href="twoFactor.otpauth_url">Open in authenticator</a
                        ><label class="form-field"
                            >Authentication code<input
                                v-model="code"
                                class="coolify-input"
                                pattern="[0-9]{6}"
                                inputmode="numeric"
                                required /></label
                        ><button
                            class="coolify-btn coolify-btn-primary"
                            type="submit"
                        >
                            Confirm
                        </button>
                        <div>
                            <strong>Recovery codes</strong>
                            <p class="page-muted">Store these codes in a secure place.</p>
                            <pre class="code-text">{{ twoFactor.recovery_codes?.join('\n') }}</pre>
                        </div>
                    </form></SettingsSection
                >
            </div></template
        >
        <template v-else-if="page === 'shared-variables' && !sharedEndpoint"
            ><div
                v-if="sharedScope === 'overview'"
                class="original-card-grid"
            >
                <a
                    v-for="[label, path, icon] in [
                        ['Team', 'team', 'teams'],
                        ['Projects', 'projects', 'projects'],
                        ['Environments', 'environments', 'layers'],
                        ['Servers', 'servers', 'servers'],
                    ]"
                    :key="path"
                    :href="`/shared-variables/${path}`"
                    class="original-resource-card"
                    ><span class="resource-glyph"><AppIcon :name="icon" /></span
                    ><strong>{{ label }}</strong>
                    <p>Manage shared variables</p></a
                >
            </div>
            <div
                v-else
                class="original-card-grid"
            >
                <a
                    v-for="item in list"
                    :key="item.uuid"
                    :href="`/shared-variables/${sharedScope === 'projects' ? 'project' : 'server'}/${item.uuid}`"
                    class="original-resource-card"
                    ><strong>{{ item.name }}</strong>
                    <p>{{ item.description }}</p></a
                >
            </div></template
        >
        <template v-else
            ><div class="list-controls">
                <div class="list-search">
                    <AppIcon name="search" /><input
                        v-model="search"
                        type="search"
                        :placeholder="
                            page === 'team' && subpage === 'members'
                                ? 'Search members'
                                : page === 'team'
                                  ? 'Search audit log'
                                  : `Search ${page === 'tags' ? 'tags' : 'variables'}`
                        "
                        aria-label="Search"
                    />
                </div>
            </div>
            <div class="original-table-scroll">
                <table class="original-table">
                    <thead>
                        <tr>
                            <template v-if="page === 'team' && subpage === 'members'"
                                ><th>Name</th>
                                <th>Email</th>
                                <th>Role</th></template
                            ><template v-else-if="page === 'team'"
                                ><th>Actor</th>
                                <th>Activity</th>
                                <th>Source</th>
                                <th>Time</th></template
                            ><template v-else
                                ><th>Name</th>
                                <th>{{ page === 'tags' ? 'Resources' : 'Value' }}</th></template
                            >
                        </tr>
                    </thead>
                    <tbody>
                        <tr
                            v-for="item in filtered"
                            :key="item.id ?? item.uuid"
                        >
                            <template v-if="page === 'team' && subpage === 'members'"
                                ><td>{{ item.name }}</td>
                                <td>{{ item.email }}</td>
                                <td>{{ item.role }}</td></template
                            ><template v-else-if="page === 'team'"
                                ><td>{{ item.user_email ?? item.user_id }}</td>
                                <td>{{ item.action }}</td>
                                <td>{{ item.resource_type }}</td>
                                <td>{{ item.created_at }}</td></template
                            ><template v-else
                                ><td>
                                    <a
                                        v-if="page === 'tags'"
                                        :href="`/tags/${encodeURIComponent(item.name)}`"
                                        >{{ item.name }}</a
                                    ><span
                                        v-else
                                        class="code-text"
                                        >{{ item.key }}</span
                                    >
                                </td>
                                <td>
                                    <input
                                        v-if="page === 'shared-variables'"
                                        type="password"
                                        :value="item.value"
                                        readonly
                                        class="coolify-input"
                                        :aria-label="`${item.key} value`"
                                    /><span v-else>{{ item.resources_count ?? '-' }}</span>
                                </td></template
                            >
                        </tr>
                    </tbody>
                </table>
            </div>
            <p
                v-if="!filtered.length"
                class="original-empty"
            >
                No results found
            </p></template
        >
        <dialog
            ref="dialog"
            class="workspace-dialog"
        >
            <form
                class="dialog-form"
                @submit.prevent="create"
            >
                <div class="dialog-heading">
                    <h2>
                        {{
                            page === 'team'
                                ? 'Invite member'
                                : page === 'tags'
                                  ? 'New tag'
                                  : 'New shared variable'
                        }}
                    </h2>
                    <button
                        type="button"
                        class="chrome-button"
                        aria-label="Close"
                        @click="dialog?.close()"
                    >
                        ✕
                    </button>
                </div>
                <template v-if="page === 'team'"
                    ><label class="form-field"
                        >Email<input
                            v-model="invitationEmail"
                            type="email"
                            class="coolify-input"
                            required /></label
                    ><label class="form-field"
                        >Role<select
                            v-model="invitationRole"
                            class="coolify-input"
                        >
                            <option value="member">Member</option>
                            <option value="admin">Admin</option>
                        </select></label
                    ></template
                ><template v-else
                    ><label class="form-field"
                        >Name<input
                            v-model="name"
                            class="coolify-input"
                            required /></label
                    ><label
                        v-if="page === 'shared-variables'"
                        class="form-field"
                        >Value<textarea
                            v-model="value"
                            class="coolify-input"
                        /></label
                ></template>
                <p
                    v-if="error"
                    role="alert"
                    class="page-error"
                >
                    {{ error }}
                </p>
                <button
                    type="submit"
                    class="coolify-btn coolify-btn-primary"
                    :disabled="busy"
                >
                    {{ page === 'team' ? 'Invite' : 'Save' }}
                </button>
            </form>
        </dialog>
    </section>
</template>
