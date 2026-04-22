/**
 * Normalize an unknown error value into a human-readable string suitable
 * for toasts, banners, and status messages.
 *
 * Template-interpolating a raw `unknown` err produces "[object Object]"
 * for plain objects and "Error: <message>" for Error instances — neither
 * is appropriate for user-facing surfaces. This helper picks the
 * friendliest representation available:
 *
 *   - `Error`: return `.message` (without the "Error:" prefix).
 *   - `string`: return as-is.
 *   - `{ message: string }`: return that message (covers thrown objects
 *     from some binding layers that don't extend Error).
 *   - anything else: `String(err)`.
 *
 * Never returns "[object Object]"; falls back to the type name instead.
 */
export function errString(err: unknown): string {
  if (err instanceof Error) {
    return err.message;
  }
  if (typeof err === 'string') {
    return err;
  }
  if (err && typeof err === 'object') {
    const maybe = (err as { message?: unknown }).message;
    if (typeof maybe === 'string' && maybe.length > 0) {
      return maybe;
    }
  }
  // String() on null/undefined produces "null"/"undefined", which is
  // accurate enough for debug breadcrumbs in a toast.
  return String(err);
}
