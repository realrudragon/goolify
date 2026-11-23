export interface NavigationEntry {
    label: string;
    icon: string;
    href?: string;
    id?: string;
    instanceOnly?: boolean;
    adminOnly?: boolean;
    children?: NavigationEntry[];
}
export const navigation: Array<{ label?: string; entries: NavigationEntry[] }> = [
    {
        entries: [
            { label: 'Dashboard', icon: 'dashboard', href: '/' },
            { label: 'Projects', icon: 'projects', href: '/projects' },
            {
                label: 'Monitoring',
                icon: 'graph',
                id: 'monitoring',
                children: [
                    { label: 'Analytics', icon: 'analytics', href: '/analytics' },
                    { label: 'Metrics', icon: 'graph', href: '/metrics' },
                    { label: 'Security', icon: 'shield-alert', href: '/vulnerabilities' },
                ],
            },
            { label: 'Terminal', icon: 'browser-terminal', href: '/terminal' },
        ],
    },
    {
        label: 'Infrastructure',
        entries: [
            { label: 'Servers', icon: 'servers', href: '/servers' },
            {
                label: 'Integrations',
                icon: 'link',
                id: 'integrations',
                children: [
                    { label: 'Sources', icon: 'sources', href: '/sources' },
                    { label: 'Destinations', icon: 'destinations', href: '/destinations' },
                    { label: 'Registries', icon: 'layers', href: '/registries', adminOnly: true },
                    { label: 'Storage', icon: 'storages', href: '/storages' },
                ],
            },
            {
                label: 'Shared Variables',
                icon: 'variables',
                id: 'variables',
                children: [
                    { label: 'Overview', icon: 'dashboard', href: '/shared-variables' },
                    { label: 'Team', icon: 'teams', href: '/shared-variables/team' },
                    { label: 'Projects', icon: 'projects', href: '/shared-variables/projects' },
                    {
                        label: 'Environments',
                        icon: 'layers',
                        href: '/shared-variables/environments',
                    },
                    { label: 'Servers', icon: 'servers', href: '/shared-variables/servers' },
                ],
            },
        ],
    },
    {
        label: 'Manage',
        entries: [
            {
                label: 'Team',
                icon: 'teams',
                id: 'team',
                children: [
                    { label: 'General', icon: 'settings', href: '/team' },
                    { label: 'Members', icon: 'teams', href: '/team/members' },
                    {
                        label: 'Audit log',
                        icon: 'time-back',
                        href: '/team/audit-log',
                        adminOnly: true,
                    },
                    { label: 'Image Scanning', icon: 'shield-alert', href: '/team/image-scanning' },
                    {
                        label: 'Danger Zone',
                        icon: 'shield-alert',
                        href: '/team/danger',
                        adminOnly: true,
                    },
                ],
            },
            {
                label: 'Notifications',
                icon: 'notifications',
                id: 'notifications',
                children: ['Email', 'Discord', 'Telegram', 'Slack', 'Pushover', 'Webhook'].map(
                    (label) => ({
                        label,
                        icon: 'notifications',
                        href: `/notifications/${label.toLowerCase()}`,
                    }),
                ),
            },
            {
                label: 'Keys & Tokens',
                icon: 'keys',
                id: 'keys',
                children: [
                    { label: 'Private Keys', icon: 'keys', href: '/security/private-key' },
                    { label: 'API Tokens', icon: 'code', href: '/security/api-tokens' },
                    {
                        label: 'Cloud Tokens',
                        icon: 'cloud',
                        href: '/security/cloud-tokens',
                        adminOnly: true,
                    },
                    {
                        label: 'Integration Tokens',
                        icon: 'network',
                        href: '/security/integration-tokens',
                        adminOnly: true,
                    },
                    {
                        label: 'Cloud-Init Scripts',
                        icon: 'file-content',
                        href: '/security/cloud-init-scripts',
                        adminOnly: true,
                    },
                ],
            },
            { label: 'Tags', icon: 'tags', href: '/tags' },
            {
                label: 'Settings',
                icon: 'settings',
                id: 'settings',
                instanceOnly: true,
                children: [
                    { label: 'General', icon: 'settings', href: '/settings' },
                    { label: 'Advanced', icon: 'layers', href: '/settings/advanced' },
                    { label: 'Updates', icon: 'refresh3', href: '/settings/updates' },
                    {
                        label: 'Image Scanning',
                        icon: 'shield-alert',
                        href: '/settings/image-scanning',
                    },
                    { label: 'Authentication', icon: 'keys', href: '/settings/oauth' },
                    { label: 'Email', icon: 'mail', href: '/settings/email' },
                ],
            },
        ],
    },
];

export function isClientRoutablePath(pathname: string): boolean {
    if (!pathname.startsWith('/')) return false;
    for (const prefix of [
        '/login',
        '/register',
        '/forgot-password',
        '/reset-password',
        '/auth/',
        '/app/auth/',
        '/api/',
        '/canvas/api/',
        '/terminal/ws',
        '/realtime',
    ]) {
        if (pathname === prefix || pathname.startsWith(prefix)) return false;
    }
    return true;
}

export function navigateTo(href: string, replace = false): void {
    if (typeof window === 'undefined') return;
    try {
        const url = new URL(href, window.location.origin);
        if (url.origin !== window.location.origin || !isClientRoutablePath(url.pathname)) {
            window.location.assign(href);
            return;
        }
        const target = url.pathname + url.search + url.hash;
        if (replace) {
            window.history.replaceState({}, '', target);
        } else if (window.location.pathname + window.location.search + window.location.hash !== target) {
            window.history.pushState({}, '', target);
        }
        window.dispatchEvent(
            new CustomEvent('coolify:navigate', { detail: { pathname: url.pathname } }),
        );
    } catch {
        window.location.assign(href);
    }
}

