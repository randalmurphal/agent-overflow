// A failure a row shows (a project's failed read) captured once for its
// Details and copy actions. The rows are rebuilt with every list snapshot
// or pipeline read, and a capture reads the backend log, so a capture is
// kept while the failure is the same one: the same log reference, or the
// same error object when it carries none.

import { captureError, type CapturedError } from '../../stores/errorReports.svelte';
import { TransportError } from '../../transport/wsClient';

function causeKey(cause: unknown): unknown {
  return cause instanceof TransportError && cause.detail ? `ref:${cause.detail.ref}` : cause;
}

/** Answers the capture of each cause it is given, reusing the last while
 * the cause is the same failure. */
export function causeCapture(): (cause: unknown) => CapturedError | null {
  let key: unknown = null;
  let held: CapturedError | null = null;
  return (cause) => {
    const next = causeKey(cause ?? null);
    if (next !== key) {
      key = next;
      held = cause === null || cause === undefined ? null : captureError(cause);
    }
    return held;
  };
}
