<script setup lang="ts">
import { onMounted, ref } from 'vue';
import { apiRequest } from '../../lib/api';
import { notifyTeamSwitch, usePageLifecycle } from '../../lib/use-page-lifecycle';

export interface TeamInfo {
  id: number;
  name: string;
  role?: string;
}

interface MeResponse {
  user_id: number;
  email: string;
  session_team_id: number;
  is_instance_admin: boolean;
  teams: TeamInfo[];
}

const props = withDefaults(defineProps<{ currentTeamId?: number }>(), {
  currentTeamId: undefined,
});

const emit = defineEmits<{ switched: [teamId: number] }>();

const lifecycle = usePageLifecycle();
const activeTeam = ref<number | null>(props.currentTeamId ?? null);
const userEmail = ref<string>('');
const teams = ref<TeamInfo[]>([]);
const switching = ref(false);
const error = ref('');

async function loadSession() {
  error.value = '';
  try {
    const me = await apiRequest<MeResponse>('/app/auth/me', { signal: lifecycle.signal });
    userEmail.value = me.email;
    activeTeam.value = me.session_team_id;
    teams.value = me.teams ?? [];
  } catch (err) {
    if ((err as Error).name !== 'AbortError') {
      error.value = (err as Error).message || 'Unable to load session';
    }
  }
}

async function switchTeam(teamId: number) {
  if (switching.value || activeTeam.value === teamId) return;
  switching.value = true;
  error.value = '';
  try {
    const res = await apiRequest<{ session_team_id: number }>('/app/auth/switch-team', {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({ team_id: teamId }),
    });
    activeTeam.value = res.session_team_id;
    notifyTeamSwitch(res.session_team_id);
    emit('switched', res.session_team_id);
  } catch (err) {
    error.value = (err as Error).message || 'Team switch failed';
  } finally {
    switching.value = false;
  }
}

onMounted(() => {
  void loadSession();
});
</script>

<template>
  <div class="coolify-card">
    <div class="coolify-well" style="display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:10px;">
      <div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap;">
        <strong>Workspace</strong>
        <span v-if="userEmail" style="color:var(--ui-fg-dim);">{{ userEmail }}</span>
        <span data-testid="active-team-id">Active Team ID: {{ activeTeam ?? 'loading' }}</span>
      </div>
      <div style="display:flex;align-items:center;gap:6px;flex-wrap:wrap;">
        <button
          v-for="team in teams"
          :key="team.id"
          type="button"
          class="coolify-btn"
          :class="{ 'coolify-btn-primary': activeTeam === team.id }"
          :disabled="switching"
          :data-testid="`switch-team-${team.id}`"
          @click="switchTeam(team.id)"
        >
          {{ team.name }} ({{ team.id }})
        </button>
      </div>
      <p v-if="error" role="alert" style="margin:0;color:var(--ui-error);">{{ error }}</p>
    </div>
  </div>
</template>
