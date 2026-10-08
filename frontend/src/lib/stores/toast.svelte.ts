import { captureError, type CapturedError } from './errorReports.svelte';

export type ToastType = 'success' | 'error' | 'warning' | 'info';
export interface ToastAction { label: string; run: () => void }

interface Toast {
  id: string;
  type: ToastType;
  message: string;
  duration: number;
  action?: ToastAction;
  /** The error behind an error toast, for its details and copy actions. */
  captured?: CapturedError;
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
  captured?: CapturedError,
): string {
  const id = `toast-${++nextId}`;
  toasts = [...toasts, { id, type, message, duration, action, captured }];
  startTimer(id, duration);
  return id;
}

/**
 * An error toast that keeps the error it reports: `message` is what the
 * toast reads, and `err` is what its details and copy actions describe.
 */
export function addErrorToast(
  message: string,
  err: unknown,
  duration = 5000,
  action?: ToastAction,
): string {
  return addToast('error', message, duration, action, captureError(err, { context: message }));
}

// An actionable failure may remain until retried or explicitly dismissed.
function startTimer(id: string, duration: number): void {
  if (duration <= 0 || timers.has(id)) return;
  timers.set(id, setTimeout(() => { removeToast(id); }, duration));
}

/** Keep a toast up while the person is reading or using it. */
export function holdToast(id: string): void {
  const timer = timers.get(id);
  if (!timer) return;
  clearTimeout(timer);
  timers.delete(id);
}

/** Restart a held toast's full dismiss delay. */
export function releaseToast(id: string): void {
  const toast = toasts.find((t) => t.id === id);
  if (toast) startTimer(id, toast.duration);
}

export function removeToast(id: string): void {
  holdToast(id);
  toasts = toasts.filter((t) => t.id !== id);
}

/** Test-only: no toasts, no dismiss timers, ids from the start. */
export function resetToastsForTest(): void {
  for (const timer of timers.values()) clearTimeout(timer);
  timers.clear();
  toasts = [];
  nextId = 0;
}
