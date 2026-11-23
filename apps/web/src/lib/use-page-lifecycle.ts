import { onUnmounted } from 'vue';

export interface LoadToken {
    readonly signal: AbortSignal;
    readonly isCurrent: () => boolean;
}

export interface PageLifecycle {
    readonly signal: AbortSignal;
    beginLoad: () => LoadToken;
    registerCleanup: (fn: () => void) => void;
    onTeamSwitch: (cb: (newTeamId: number) => void) => void;
}

const teamListeners = new Set<(newTeamId: number) => void>();
let restoreHandlerInstalled = false;

export function notifyTeamSwitch(newTeamId: number): void {
    for (const listener of teamListeners) {
        listener(newTeamId);
    }
}

export function usePageLifecycle(): PageLifecycle {
    const controller = new AbortController();
    const cleanups: Array<() => void> = [];
    let generation = 0;
    let activeLoadController: AbortController | undefined;

    const abortActiveLoad = () => {
        generation++;
        if (activeLoadController && !activeLoadController.signal.aborted) {
            activeLoadController.abort();
        }
        activeLoadController = undefined;
    };

    const teardown = () => {
        abortActiveLoad();
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
        if (!restoreHandlerInstalled) {
            restoreHandlerInstalled = true;
            window.addEventListener('pageshow', (event) => {
                // A page restored from back/forward cache contains aborted controllers.
                if (event.persisted) window.location.reload();
            });
        }
        window.addEventListener('pagehide', teardown, { once: true });
        cleanups.push(() => window.removeEventListener('pagehide', teardown));
    }

    onUnmounted(teardown);

    return {
        signal: controller.signal,
        beginLoad(): LoadToken {
            abortActiveLoad();
            const currentGen = generation;
            const loadCtrl = new AbortController();
            activeLoadController = loadCtrl;
            if (controller.signal.aborted) {
                loadCtrl.abort();
            }
            return {
                signal: loadCtrl.signal,
                isCurrent: () => currentGen === generation && !loadCtrl.signal.aborted && !controller.signal.aborted,
            };
        },
        registerCleanup(fn: () => void) {
            cleanups.push(fn);
        },
        onTeamSwitch(cb: (newTeamId: number) => void) {
            const wrapped = (newTeamId: number) => {
                abortActiveLoad();
                cb(newTeamId);
            };
            teamListeners.add(wrapped);
            cleanups.push(() => teamListeners.delete(wrapped));
        },
    };
}
