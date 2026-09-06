export type ToastType = 'success' | 'error' | 'warning' | 'info';
export interface ToastAction { label: string; run: () => void }

interface Toast {
  id: string;
  type: ToastType;
  message: string;
  duration: number;
  action?: ToastAction;
}

let nextId = 0;

let toasts = $state<Toast[]>([]);
const timers = new Map<string, ReturnType<typeof setTimeout>>();

export function getToasts(): Toast[] {
  return toasts;
}

export function addToast(
  type: ToastType,
  message: string,
  duration = 5000,
  action?: ToastAction,
): string {
  const id = `toast-${++nextId}`;
  toasts = [...toasts, { id, type, message, duration, action }];

  // An actionable failure may remain until retried or explicitly dismissed.
  if (duration > 0) {
    const timer = setTimeout(() => { removeToast(id); }, duration);
    timers.set(id, timer);
  }

  return id;
}

export function removeToast(id: string): void {
  const timer = timers.get(id);
  if (timer) {
    clearTimeout(timer);
    timers.delete(id);
  }
  toasts = toasts.filter((t) => t.id !== id);
}
