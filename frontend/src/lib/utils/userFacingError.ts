// userFacingError trims Go wrap chains, drops UUIDs, and normalizes
// connection / shutdown errors so toasts read like product strings,
// not log lines. Call from every catch path that hands an error to
// addToast.
//
// The Go wrap convention is `outer: middle: inner`; the inner segment
// is usually the message the user actually needs. We keep the last
// segment unless it's so short (<= 6 chars) that dropping the prefix
// would lose useful context — e.g. `bad request: timeout` reduces to
// `Timeout.` after capitalisation, which is fine, but `db: i/o` would
// reduce to `I/o.`, which loses the cause.
//
// UUIDs are stripped wholesale: nobody hand-types a thread id, and
// surfacing them in a toast just adds visual noise. The `\s*` before
// the UUID is intentional so `for thread <uuid>` collapses to
// `for thread` rather than `for thread ` with a trailing space.

export function userFacingError(err: unknown, fallback = 'Something went wrong.'): string {
  if (err === null || err === undefined) return fallback;
  let raw = '';
  if (err instanceof Error) {
    raw = err.message;
  } else if (typeof err === 'string') {
    raw = err;
  } else if (typeof err === 'object') {
    const maybe = (err as { message?: unknown }).message;
    if (typeof maybe === 'string') {
      raw = maybe;
    } else {
      raw = String(err);
    }
  } else {
    raw = String(err);
  }
  if (!raw) return fallback;
  const parts = raw.split(/:\s+/);
  if (parts.length > 1 && parts[parts.length - 1].length > 6) {
    raw = parts[parts.length - 1];
  }
  raw = raw.replace(/\s*[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/gi, '');
  raw = raw.trim();
  if (!raw) return fallback;
  raw = raw.charAt(0).toUpperCase() + raw.slice(1);
  if (!/[.!?]$/.test(raw)) raw += '.';
  return raw;
}
