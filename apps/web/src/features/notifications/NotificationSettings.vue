<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue';
import SettingsSection from '../../components/SettingsSection.vue';
import { apiRequest } from '../../lib/api';
import { loadSession, session } from '../../lib/session';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

const props = withDefaults(defineProps<{ channel?: string }>(), { channel: 'email' });
interface Field {
    key: string;
    label: string;
    type?: string;
}
const channels: Record<string, { title: string; helper: string; fields: Field[] }> = {
    discord: {
        title: 'Discord',
        helper: 'Send team notifications to a Discord channel through an incoming webhook.',
        fields: [{ key: 'webhook_url', label: 'Webhook URL', type: 'password' }],
    },
    slack: {
        title: 'Slack',
        helper: 'Send team notifications to a Slack channel through an incoming webhook.',
        fields: [{ key: 'webhook_url', label: 'Webhook URL', type: 'password' }],
    },
    webhook: {
        title: 'Webhook',
        helper: 'Send team notifications to a custom webhook endpoint.',
        fields: [{ key: 'webhook_url', label: 'Webhook URL', type: 'password' }],
    },
    telegram: {
        title: 'Telegram',
        helper: 'Send team notifications through your Telegram bot.',
        fields: [
            { key: 'bot_token', label: 'Bot Token', type: 'password' },
            { key: 'chat_id', label: 'Chat ID' },
        ],
    },
    pushover: {
        title: 'Pushover',
        helper: 'Send team notifications to your Pushover devices.',
        fields: [
            { key: 'pushover_user_key', label: 'User Key', type: 'password' },
            { key: 'pushover_api_token', label: 'API Token', type: 'password' },
        ],
    },
    email: {
        title: 'Email',
        helper: 'Configure email delivery for this team.',
        fields: [
            { key: 'smtp_host', label: 'Host' },
            { key: 'smtp_port', label: 'Port', type: 'number' },
            { key: 'smtp_username', label: 'Username' },
            { key: 'smtp_password', label: 'Password', type: 'password' },
            { key: 'smtp_from', label: 'From Address', type: 'email' },
            { key: 'smtp_recipient', label: 'Recipients' },
        ],
    },
};
const channel = computed(() => channels[props.channel]);
const lifecycle = usePageLifecycle();
const config = ref<Record<string, any>>({ enabled: false });
const initial = ref<Record<string, any>>({});
let savedFields = new Set<string>();
const teamId = ref<number | null>(null);
const loading = ref(true);
const saving = ref(false);
const error = ref('');
const status = ref('');
const canEdit = computed(
    () =>
        teamId.value !== null &&
        ['admin', 'owner'].includes(
            session.me?.teams.find((team) => team.id === teamId.value)?.role?.toLowerCase() ?? '',
        ),
);
const eventPrefs = ref<Record<string, boolean>>({});
const initialEvents = ref<Record<string, boolean>>({});
const eventList: [string, string][] = [
    ['deployment_success', 'Deployment Success'],
    ['deployment_failure', 'Deployment Failure'],
    ['status_change', 'Container Status Change'],
    ['backup_success', 'Backup Success'],
    ['backup_failure', 'Backup Failure'],
    ['scheduled_task_success', 'Scheduled Task Success'],
    ['scheduled_task_failure', 'Scheduled Task Failure'],
    ['server_disk_usage', 'Server High Disk Usage'],
    ['server_reachable', 'Server Reachable'],
    ['server_unreachable', 'Server Unreachable'],
];
const dirty = computed(
    () =>
        JSON.stringify(config.value) !== JSON.stringify(initial.value) ||
        JSON.stringify(eventPrefs.value) !== JSON.stringify(initialEvents.value),
);
let generation = 0;
let controller: AbortController | undefined;
lifecycle.registerCleanup(() => {
    generation++;
    controller?.abort();
});
async function load() {
    const current = ++generation;
    controller?.abort();
    controller = new AbortController();
    loading.value = true;
    error.value = '';
    config.value = { enabled: false };
    teamId.value = null;
    if (!channel.value) {
        loading.value = false;
        return;
    }
    try {
        await loadSession();
        const saved = await apiRequest<Record<string, any>>(
            `/api/v1/notifications/${props.channel}`,
            { signal: controller.signal },
        );
        if (current !== generation || lifecycle.signal.aborted) return;
        teamId.value = saved.team_id;
        savedFields = new Set(Object.keys(saved));
        const fields: Record<string, any> = { enabled: Boolean(saved.enabled) };
        for (const field of channel.value.fields)
            fields[field.key] =
                saved[field.key] ?? (field.type === 'number' && canEdit.value ? 587 : '');
        config.value = fields;
        initial.value = { ...fields };
        const evs: Record<string, boolean> = {};
        for (const [k] of eventList) {
            evs[k] = Boolean(saved.events?.[k]);
        }
        eventPrefs.value = evs;
        initialEvents.value = { ...evs };
    } catch (err) {
        if (current === generation && !controller.signal.aborted)
            error.value = (err as Error).message;
    } finally {
        if (current === generation) loading.value = false;
    }
}
async function save() {
    if (!canEdit.value || saving.value || !dirty.value || !channel.value) return;
    saving.value = true;
    error.value = '';
    status.value = '';
    try {
        const changed = Object.fromEntries(
            Object.entries(config.value).filter(
                ([key, value]) => value !== initial.value[key] || !savedFields.has(key),
            ),
        );
        await apiRequest(`/api/v1/notifications/${props.channel}`, {
            method: 'PATCH',
            body: JSON.stringify({
                ...changed,
                events: eventPrefs.value,
                expected_team_id: teamId.value,
            }),
        });
        await load();
        if (!error.value) status.value = 'Saved.';
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        saving.value = false;
    }
}
async function sendTest() {
    if (!canEdit.value || saving.value || dirty.value || !config.value.enabled) return;
    saving.value = true;
    error.value = '';
    status.value = '';
    try {
        await apiRequest('/api/v1/notifications/test', {
            method: 'POST',
            body: JSON.stringify({
                channel: props.channel,
                event: 'test',
                summary: 'Goolify test notification',
                expected_team_id: teamId.value,
            }),
        });
        status.value = 'Test notification sent.';
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        saving.value = false;
    }
}
lifecycle.onTeamSwitch(() => {
    status.value = '';
    void load();
});
watch(
    () => props.channel,
    () => {
        status.value = '';
        void load();
    },
);
onMounted(load);
</script>

<template>
    <section
        class="original-page"
        data-testid="notification-settings"
        :data-channel="channel ? props.channel : undefined"
    >
        <header class="notification-mobile-heading">
            <h1>Notifications</h1>
            <p>Configure how your team receives deployment and system alerts.</p>
        </header>
        <p
            v-if="!channel"
            role="alert"
            class="page-error"
        >
            Notification channel not found.
        </p>
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
        <p
            v-if="loading"
            role="status"
            class="page-muted"
        >
            Loading notification settings…
        </p>
        <form
            v-else-if="channel"
            @submit.prevent="save"
        >
            <div
                v-if="dirty && canEdit"
                class="unsaved-bar"
            >
                <span>You have unsaved changes</span>
                <div class="page-actions">
                    <button
                        type="button"
                        class="coolify-btn"
                        @click="load"
                    >
                        Discard</button
                    ><button
                        type="submit"
                        class="coolify-btn coolify-btn-primary"
                        :disabled="saving"
                    >
                        Save
                    </button>
                </div>
            </div>
            <SettingsSection
                :title="channel.title"
                :helper="channel.helper"
            >
                <template #actions
                    ><label class="check-field"
                        ><input
                            v-model="config.enabled"
                            type="checkbox"
                            :disabled="!canEdit || saving"
                            aria-label="Enabled"
                        />Enabled</label
                    ><button
                        v-if="canEdit"
                        type="button"
                        class="coolify-btn"
                        :disabled="saving || dirty || !config.enabled"
                        :title="
                            dirty ? 'Save changes before sending a test notification' : undefined
                        "
                        @click="sendTest"
                    >
                        Send test notification
                    </button></template
                >
                <div class="form-grid">
                    <label
                        v-for="field in channel.fields"
                        :key="field.key"
                        class="form-field"
                        :class="{ 'full-span': channel.fields.length === 1 }"
                        >{{ field.label
                        }}<input
                            v-if="field.type === 'number'"
                            v-model.number="config[field.key]"
                            type="number"
                            min="1"
                            max="65535"
                            class="coolify-input"
                            :readonly="!canEdit"
                            :aria-label="field.label" /><input
                            v-else
                            v-model="config[field.key]"
                            :type="field.type ?? 'text'"
                            class="coolify-input"
                            :readonly="!canEdit"
                            :aria-label="field.label"
                            :placeholder="
                                !canEdit && field.type === 'password' ? 'Hidden' : undefined
                            "
                            autocomplete="off"
                    /></label>
                </div>
            </SettingsSection>
            <SettingsSection
                title="Event Preferences"
                helper="Choose which events trigger notifications on this channel."
            >
                <div class="form-grid">
                    <label
                        v-for="[key, label] in eventList"
                        :key="key"
                        class="check-field"
                        ><input
                            v-model="eventPrefs[key]"
                            type="checkbox"
                            :disabled="!canEdit"
                        />{{ label }}</label
                    >
                </div>
            </SettingsSection>
        </form>
    </section>
</template>
