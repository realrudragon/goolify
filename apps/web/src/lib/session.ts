import { reactive } from 'vue';
import { apiRequest } from './api';
import { notifyTeamSwitch } from './use-page-lifecycle';

export interface SessionTeam {
    id: number;
    name: string;
    role?: string;
}
export interface SessionInfo {
    user_id: number;
    email: string;
    session_team_id: number;
    is_instance_admin: boolean;
    teams: SessionTeam[];
}
export const session = reactive<{ me: SessionInfo | null }>({ me: null });
let pending: Promise<void> | null = null;
let revision = 0;

export function loadSession(): Promise<void> {
    if (session.me) return Promise.resolve();
    if (pending) return pending;
    const current = revision;
    pending = apiRequest<SessionInfo>('/app/auth/me')
        .then((me) => {
            if (current === revision) session.me = me;
        })
        .finally(() => {
            pending = null;
        });
    return pending;
}

export async function selectSessionTeam(teamId: number): Promise<void> {
    const result = await apiRequest<{ session_team_id: number }>('/app/auth/switch-team', {
        method: 'POST',
        body: JSON.stringify({ team_id: teamId }),
    });
    revision++;
    if (session.me) session.me.session_team_id = result.session_team_id;
    notifyTeamSwitch(result.session_team_id);
}
