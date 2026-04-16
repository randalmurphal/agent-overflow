export type ToastType = 'success' | 'error' | 'warning' | 'info';

export interface Toast {
  id: string;
  type: ToastType;
  message: string;
  duration: number;
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
): string {
  const id = `toast-${++nextId}`;
  toasts = [...toasts, { id, type, message, duration }];

  const timer = setTimeout(() => {
    removeToast(id);
  }, duration);
  timers.set(id, timer);

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
