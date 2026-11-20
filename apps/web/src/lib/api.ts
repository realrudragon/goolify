export class ApiError extends Error {
  readonly status: number;
  readonly errors?: Record<string, string[]>;

  constructor(status: number, message: string, errors?: Record<string, string[]>) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.errors = errors;
  }
}

function readCSRFToken(): string {
  if (typeof document === 'undefined') return '';
  const meta = document.querySelector<HTMLMetaElement>('meta[name="csrf-token"]');
  if (meta?.content) return meta.content;
  const match = document.cookie.match(/(?:^|;\s*)coolify_csrf=([^;]+)/);
  return match ? decodeURIComponent(match[1]) : '';
}

function isMutatingMethod(method?: string): boolean {
  const upper = (method ?? 'GET').toUpperCase();
  return upper === 'POST' || upper === 'PUT' || upper === 'PATCH' || upper === 'DELETE';
}

export async function apiRequest<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  if (!headers.has('Accept')) {
    headers.set('Accept', 'application/json');
  }
  if (init.body && typeof init.body === 'string' && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json');
  }
  if (isMutatingMethod(init.method)) {
    const csrf = readCSRFToken();
    if (csrf && !headers.has('X-CSRF-TOKEN')) {
      headers.set('X-CSRF-TOKEN', csrf);
    }
  }

  const response = await fetch(path, {
    ...init,
    credentials: 'same-origin',
    headers,
  });

  if (response.status === 401 && typeof window !== 'undefined' && !window.location.pathname.startsWith('/login')) {
    window.location.assign('/login');
  }

  const contentType = response.headers.get('content-type') ?? '';
  const isJSON = contentType.includes('application/json');

  if (!response.ok) {
    if (isJSON) {
      const payload = (await response.json().catch(() => ({}))) as {
        message?: string;
        errors?: Record<string, string[]>;
      };
      throw new ApiError(response.status, payload.message ?? response.statusText, payload.errors);
    }
    const text = await response.text().catch(() => response.statusText);
    throw new ApiError(response.status, text || response.statusText);
  }

  if (response.status === 204 || !isJSON) {
    return undefined as T;
  }
  return (await response.json()) as T;
}
