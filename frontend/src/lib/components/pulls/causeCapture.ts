// A failure a list row shows (a project's failed read) captured once for
// its Details and copy actions. The rows are rebuilt with every list
// snapshot or pipeline read and remount as the virtualized list scrolls,
// and a capture reads the backend log, so one capture serves the failure
// while it is the same one: the same log reference, or the same error
// object when it carries none. The references held are bounded.

import { captureError, type CapturedError } from '../../stores/errorReports.svelte';
import { TransportError } from '../../transport/wsClient';

const MAX_REFS = 64;
const byRef = new Map<string, CapturedError>();
const byObject = new WeakMap<object, CapturedError>();

/** The capture of `cause`, made on first sight of the failure; null with none. */
export function capturedCause(cause: unknown): CapturedError | null {
  if (cause === null || cause === undefined) return null;
  const ref = cause instanceof TransportError ? cause.detail?.ref : undefined;
  if (ref !== undefined) {
    const held = byRef.get(ref);
    if (held) {
      // Most recent last, so the oldest goes first.
      byRef.delete(ref);
      byRef.set(ref, held);
      return held;
    }
    const captured = captureError(cause);
    byRef.set(ref, captured);
    if (byRef.size > MAX_REFS) byRef.delete(byRef.keys().next().value!);
    return captured;
  }
  if (typeof cause !== 'object') return captureError(cause);
  let captured = byObject.get(cause);
  if (!captured) {
    captured = captureError(cause);
    byObject.set(cause, captured);
  }
  return captured;
}

export function __resetCauseCaptureForTest(): void {
  byRef.clear();
}
