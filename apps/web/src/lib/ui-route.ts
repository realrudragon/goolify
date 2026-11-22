export type WorkspaceSection =
    | 'overview'
    | 'servers'
    | 'projects'
    | 'operations'
    | 'databases'
    | 'sources'
    | 'security'
    | 'settings';

export interface UIRoute {
    section: WorkspaceSection;
    page?: string;
    subpage?: string;
    serverUuid?: string;
    projectUuid?: string;
    environmentUuid?: string;
    applicationUuid?: string;
    serviceUuid?: string;
    databaseUuid?: string;
    destinationUuid?: string;
    storageUuid?: string;
    sourceUuid?: string;
    integrationUuid?: string;
}

export function resolveUIRoute(path: string): UIRoute {
    const segments = path.split('/').filter(Boolean);
    const root = segments[0] ?? '';
    const result: UIRoute = { section: 'overview' };
    const id = (index: number): string | undefined => {
        try {
            const value = decodeURIComponent(segments[index] ?? '');
            return /^[A-Za-z0-9_-]+$/.test(value) ? value : undefined;
        } catch {
            return undefined;
        }
    };
    if (['servers', 'server', 'destinations', 'destination', 'registries'].includes(root)) {
        result.section = 'servers';
        if (root === 'server' || (root === 'servers' && !['canvas', 'new'].includes(segments[1])))
            result.serverUuid = id(1);
    } else if (['projects', 'project', 'services', 'deployments', 'applications'].includes(root)) {
        result.section = 'projects';
        if (root === 'project' || (root === 'projects' && segments[1] !== 'canvas'))
            result.projectUuid = id(1);
        if (root === 'services') result.serviceUuid = id(1);
        if (root === 'applications') result.applicationUuid = id(1);
        if (segments[2] === 'environment') {
            result.environmentUuid = id(3);
            if (segments[4] === 'application') result.applicationUuid = id(5);
            if (segments[4] === 'service') result.serviceUuid = id(5);
            if (segments[4] === 'database') {
                result.databaseUuid = id(5);
                result.section = 'databases';
            }
        }
    } else if (['databases', 'storages'].includes(root)) {
        result.section = 'databases';
        if (root === 'databases') result.databaseUuid = id(1);
    } else if (['terminal', 'metrics', 'analytics'].includes(root)) {
        result.section = 'operations';
    } else if (
        ['sources', 'source'].includes(root) ||
        (root === 'security' && /^(cloud|integration)/.test(segments[1] ?? ''))
    ) {
        result.section = 'sources';
    } else if (root === 'security' && segments[1] === 'api-tokens') {
        result.section = 'settings';
    } else if (['security', 'vulnerabilities'].includes(root)) {
        result.section = 'security';
    } else if (
        [
            'team',
            'teams',
            'settings',
            'notifications',
            'profile',
            'transfers',
            'shared-variables',
            'tags',
            'subscription',
            'admin',
            'onboarding',
            'select-team',
            'force-password-reset',
            'two-factor-required',
        ].includes(root)
    ) {
        result.section = 'settings';
    }
    result.subpage = '';
    const indexes: Record<string, string> = {
        '': 'dashboard',
        app: 'dashboard',
        servers: 'servers',
        destinations: 'destinations',
        registries: 'registries',
        storages: 'storages',
        projects: 'projects',
        sources: 'sources',
        vulnerabilities: 'vulnerabilities',
        terminal: 'terminal',
        metrics: 'metrics',
        analytics: 'analytics',
        tags: 'tags',
        'shared-variables': 'shared-variables',
        team: 'team',
        teams: 'team',
        notifications: 'notifications',
        settings: 'settings',
        profile: 'profile',
        admin: 'admin',
        onboarding: 'onboarding',
        subscription: 'subscription',
        'select-team': 'select-team',
        'force-password-reset': 'force-password-reset',
        'two-factor-required': 'two-factor-required',
    };
    result.page = indexes[root] ?? 'not-found';
    if (['team', 'teams', 'settings', 'profile', 'notifications'].includes(root)) {
        result.subpage =
            segments.slice(1).join('/') || (root === 'notifications' ? 'email' : 'general');
    } else if (root === 'servers' && segments[1] === 'new') {
        result.page = 'server-create';
        result.subpage = segments.slice(2).join('/');
    } else if (root === 'server' || (root === 'servers' && result.serverUuid)) {
        result.page = result.serverUuid ? 'server' : 'not-found';
        result.subpage = segments.slice(2).join('/') || 'general';
    } else if (root === 'destination') {
        result.destinationUuid = id(1);
        result.page = result.destinationUuid ? 'destination' : 'not-found';
        result.subpage = segments.slice(2).join('/') || 'general';
    } else if (root === 'storages' && segments[1]) {
        result.storageUuid = id(1);
        result.page = result.storageUuid ? 'storage' : 'not-found';
        result.subpage = segments.slice(2).join('/') || 'general';
    } else if (root === 'source') {
        result.sourceUuid = id(2);
        result.page = result.sourceUuid ? 'source' : 'not-found';
        result.subpage = segments.slice(3).join('/') || 'general';
    } else if (root === 'security') {
        result.page =
            (
                {
                    'private-key': 'private-keys',
                    'api-tokens': 'api-tokens',
                    'cloud-tokens': 'cloud-tokens',
                    'integration-tokens': 'integration-tokens',
                    'cloud-init-scripts': 'cloud-init-scripts',
                } as Record<string, string>
            )[segments[1]] ?? 'security';
        if (segments[2] && segments[2] !== 'new') {
            result.integrationUuid = id(2);
            if (!result.integrationUuid) result.page = 'not-found';
            result.subpage = segments.slice(3).join('/') || 'general';
        } else result.subpage = segments[2] === 'new' ? 'new' : '';
    } else if (
        root === 'project' ||
        root === 'applications' ||
        root === 'services' ||
        root === 'databases'
    ) {
        result.page = result.applicationUuid
            ? 'application'
            : result.serviceUuid
              ? 'service'
              : result.databaseUuid
                ? 'database'
                : result.environmentUuid
                  ? 'environment'
                  : result.projectUuid
                    ? 'project'
                    : 'not-found';
        const offset =
            root === 'project'
                ? result.applicationUuid || result.serviceUuid || result.databaseUuid
                    ? 6
                    : result.environmentUuid
                      ? 4
                      : 2
                : 2;
        result.subpage = segments.slice(offset).join('/') || 'general';
    } else if (segments.includes('canvas')) {
        result.page = 'canvas';
        result.subpage = root;
    } else if (root === 'shared-variables') {
        result.subpage = segments.slice(1).join('/');
    }
    return result;
}
