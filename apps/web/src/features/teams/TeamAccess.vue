<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue';
import { session, loadSession, selectSessionTeam, type SessionTeam } from '../../lib/session';
export type TeamInfo = SessionTeam;
defineProps<{ currentTeamId?: number }>();
const emit = defineEmits<{ switched: [teamId: number] }>();
const activeTeam = computed(() =>
    session.me?.teams.find((team) => team.id === session.me?.session_team_id),
);
const switching = ref(false);
const error = ref('');
const menu = ref<HTMLDetailsElement>();
async function switchTeam(teamId: number) {
    if (switching.value || session.me?.session_team_id === teamId) return;
    switching.value = true;
    error.value = '';
    try {
        await selectSessionTeam(teamId);
        emit('switched', session.me?.session_team_id ?? teamId);
        if (menu.value) menu.value.open = false;
        // The original switcher returns to the new team's dashboard.
        window.location.assign('/');
    } catch (err) {
        error.value = (err as Error).message;
    } finally {
        switching.value = false;
    }
}
function dismiss(event: MouseEvent | KeyboardEvent) {
    if (!menu.value?.open) return;
    if (event instanceof KeyboardEvent && event.key === 'Escape') {
        menu.value.open = false;
        menu.value.querySelector('summary')?.focus();
    } else if (
        event instanceof MouseEvent &&
        event.target instanceof Node &&
        !menu.value.contains(event.target)
    )
        menu.value.open = false;
}
onMounted(() => {
    void loadSession().catch((err) => {
        error.value = (err as Error).message;
    });
    window.addEventListener('click', dismiss);
    window.addEventListener('keydown', dismiss);
});
onUnmounted(() => {
    window.removeEventListener('click', dismiss);
    window.removeEventListener('keydown', dismiss);
});
</script>
<template>
    <div
        class="team-switcher"
        data-testid="team-switcher"
    >
        <span
            hidden
            data-testid="active-team-id"
            :data-team-id="session.me?.session_team_id"
        ></span>
        <details
            ref="menu"
            class="team-menu"
        >
            <summary>
                <span class="team-avatar">{{ activeTeam?.name[0]?.toUpperCase() ?? 'T' }}</span
                ><span class="team-name">{{ activeTeam?.name ?? 'Select team' }}</span
                ><span
                    class="team-chevron"
                    aria-hidden="true"
                    >⌃<br />⌄</span
                >
            </summary>
            <div class="team-popover">
                <button
                    v-for="team in session.me?.teams"
                    :key="team.id"
                    type="button"
                    :disabled="switching"
                    :data-testid="`switch-team-${team.id}`"
                    :aria-pressed="team.id === session.me?.session_team_id"
                    @click="switchTeam(team.id)"
                >
                    <span class="team-avatar">{{ team.name[0]?.toUpperCase() }}</span
                    ><span>{{ team.name }}</span
                    ><span
                        v-if="team.id === session.me?.session_team_id"
                        aria-hidden="true"
                        >✓</span
                    >
                </button>
                <a href="/teams">Manage teams</a>
            </div>
        </details>
        <p
            v-if="error"
            role="alert"
            class="team-error"
        >
            {{ error }}
        </p>
    </div>
</template>
