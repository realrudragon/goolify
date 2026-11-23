import { reactive } from 'vue';

export interface ToastItem {
    id: number;
    message: string;
    kind: 'success' | 'error' | 'info';
}

let nextToastId = 1;

export const toastState = reactive<{ items: ToastItem[] }>({
    items: [],
});

export function pushToast(message: string, kind: 'success' | 'error' | 'info' = 'success'): void {
    const id = nextToastId++;
    toastState.items.push({ id, message, kind });
    if (typeof window !== 'undefined') {
        window.setTimeout(() => {
            dismissToast(id);
        }, 4200);
    }
}

export function dismissToast(id: number): void {
    const idx = toastState.items.findIndex((t) => t.id === id);
    if (idx !== -1) toastState.items.splice(idx, 1);
}
