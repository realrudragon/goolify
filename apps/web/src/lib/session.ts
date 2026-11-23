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
const SESSION_CACHE_KEY = 'coolify_session_display';

function readCachedSession(): SessionInfo | null {
    if (typeof window === 'undefined' || typeof sessionStorage === 'undefined') return null;
    try {
        const raw = sessionStorage.getItem(SESSION_CACHE_KEY);
        return raw ? (JSON.parse(raw) as SessionInfo) : null;
    } catch {
        return null;
    }
}

function writeCachedSession(me: SessionInfo | null): void {
    if (typeof window === 'undefined' || typeof sessionStorage === 'undefined') return;
    try {
        if (me) sessionStorage.setItem(SESSION_CACHE_KEY, JSON.stringify(me));
        else sessionStorage.removeItem(SESSION_CACHE_KEY);
    } catch {
        // Ignore storage errors
    }
}

export const session = reactive<{ me: SessionInfo | null }>({ me: readCachedSession() });
let pending: Promise<void> | null = null;
let validated = false;
let revision = 0;

export function loadSession(): Promise<void> {
    if (session.me && validated) return Promise.resolve();
    if (pending) return pending;
    const current = revision;
    pending = apiRequest<SessionInfo>('/app/auth/me')
        .then((me) => {
            if (current === revision) {
                session.me = me;
                validated = true;
                writeCachedSession(me);
            }
        })
        .catch((err) => {
            session.me = null;
            validated = false;
            writeCachedSession(null);
            throw err;
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
    if (session.me) {
        session.me.session_team_id = result.session_team_id;
        writeCachedSession(session.me);
    }
    notifyTeamSwitch(result.session_team_id);
}
