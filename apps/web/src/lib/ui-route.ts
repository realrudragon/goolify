export type WorkspaceSection = 'overview' | 'servers' | 'projects' | 'operations' | 'databases' | 'sources' | 'security' | 'settings';

export interface UIRoute {
  section: WorkspaceSection;
  serverUuid?: string;
  projectUuid?: string;
  environmentUuid?: string;
  applicationUuid?: string;
  serviceUuid?: string;
  databaseUuid?: string;
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
    if (root === 'server' || (root === 'servers' && !['canvas', 'new'].includes(segments[1]))) result.serverUuid = id(1);
  } else if (['projects', 'project', 'services', 'deployments', 'applications'].includes(root)) {
    result.section = 'projects';
    if (root === 'project' || (root === 'projects' && segments[1] !== 'canvas')) result.projectUuid = id(1);
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
  } else if (['sources', 'source'].includes(root) || (root === 'security' && /^(cloud|integration)/.test(segments[1] ?? ''))) {
    result.section = 'sources';
  } else if (root === 'security' && segments[1] === 'api-tokens') {
    result.section = 'settings';
  } else if (['security', 'vulnerabilities'].includes(root)) {
    result.section = 'security';
  } else if (['team', 'teams', 'settings', 'notifications', 'profile', 'transfers', 'shared-variables', 'tags', 'subscription', 'admin', 'onboarding', 'select-team', 'force-password-reset', 'two-factor-required'].includes(root)) {
    result.section = 'settings';
  }
  return result;
}
