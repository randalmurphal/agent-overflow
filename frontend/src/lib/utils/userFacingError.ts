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
// `for thread` rather than `for thread ` with a trailing space, and quotes
// around one go with it. The full text stays in the error's report
// (errorReport.ts), which is where a person reads ids.
//
// A backend failure that carries its wrap chain (TransportError.detail)
// reads from the chain's innermost layer instead of re-splitting the
// message, which the transport clamps. A reviewed message
// (errorsx.Public) is already the sentence to show and is not split.

import { scopeRefusalMessage } from '../transport/scopeRefusal';
import { TransportError } from '../transport/wsClient';

// Codes whose message is internal prose rather than a reviewed sentence.
const UNREVIEWED_CODES = new Set(['method_error', 'internal', 'temporarily_unavailable']);

export function userFacingError(err: unknown, fallback = 'Something went wrong.'): string {
  if (err === null || err === undefined) return fallback;
  // Authorization refusals carry the missing capability as a wire FIELD
  // (scope_required / step_up_required), and the one presentation module
  // phrases it — running those through the wrap-trimmer below would
  // instead surface the tail of a server sentence and drift per surface.
  const refusal = scopeRefusalMessage(err);
  if (refusal !== null) return refusal;
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
  const detail = err instanceof TransportError ? err.detail : undefined;
  if (detail) {
    // A reviewed message is shown whole; internal prose reads from the
    // innermost cause, which an off-host caller does not receive.
    if (UNREVIEWED_CODES.has((err as TransportError).code)) {
      raw = detail.chain.at(-1) ?? 'The backend could not complete this request';
    }
  } else {
    const parts = raw.split(/:\s+/);
    if (parts.length > 1 && parts[parts.length - 1].length > 6) {
      raw = parts[parts.length - 1];
    }
  }
  raw = raw.replace(/\s*"?[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"?/gi, '');
  raw = raw.trim();
  if (!raw) return fallback;
  raw = raw.charAt(0).toUpperCase() + raw.slice(1);
  if (!/[.!?]$/.test(raw)) raw += '.';
  return raw;
}
