import { onUnmounted } from 'vue';

export interface PageLifecycle {
  readonly signal: AbortSignal;
  registerCleanup: (fn: () => void) => void;
  onTeamSwitch: (cb: (newTeamId: number) => void) => void;
}

const teamListeners = new Set<(newTeamId: number) => void>();

export function notifyTeamSwitch(newTeamId: number): void {
  for (const listener of teamListeners) {
    listener(newTeamId);
  }
}

export function usePageLifecycle(): PageLifecycle {
  const controller = new AbortController();
  const cleanups: Array<() => void> = [];

  const teardown = () => {
    if (!controller.signal.aborted) {
      controller.abort();
    }
    while (cleanups.length > 0) {
      const fn = cleanups.pop();
      try {
        fn?.();
      } catch {
        // Ignore teardown errors during page unload
      }
    }
  };

  if (typeof window !== 'undefined') {
    window.addEventListener('pagehide', teardown, { once: true });
    cleanups.push(() => window.removeEventListener('pagehide', teardown));
  }

  onUnmounted(teardown);

  return {
    signal: controller.signal,
    registerCleanup(fn: () => void) {
      cleanups.push(fn);
    },
    onTeamSwitch(cb: (newTeamId: number) => void) {
      teamListeners.add(cb);
      cleanups.push(() => teamListeners.delete(cb));
    },
  };
}
