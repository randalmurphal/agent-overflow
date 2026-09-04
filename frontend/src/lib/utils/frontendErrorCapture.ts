import { ReportFrontendErrorBatch } from '../stores/bindings';
import { isMethodUnavailableError } from '../stores/transportStatus.svelte';
import { wsClient } from '../transport/wsClient';
import { UI_TRACE_MAX_LINE_BYTES } from './uiTraceLimits';
import { redactDiagnosticText } from './diagnosticRedaction';

/*
 * Always-on global error capture. Window `error` and `unhandledrejection`
 * events, and the document's `securitypolicyviolation` events, are
 * serialized to JSONL and appended (batched) to
 * <configDir>/ui-trace/frontend-errors.jsonl via ReportFrontendErrorBatch.
 *
 * Why this exists: a render-path exception in Svelte aborts the updating
 * reaction after any freshly-created deriveds it read have already been
 * connected to their dependency signals — the aborted reader never
 * registers as their reaction, so those deriveds leak permanently (they
 * survive component destroy and even unmount of the whole app). One
 * recurring silent throw turned into a 100+ MB heap over a long session.
 * Errors here are user-facing state, not log noise — they must land
 * somewhere a human can find after the fact.
 */

interface CapturedErrorRecord {
  at: number;
  kind: 'error' | 'unhandledrejection' | 'csp' | 'diagnostic';
  message: string;
  stack: string;
  /** script URL for `error` events; empty for rejections. */
  source: string;
  line: number;
  col: number;
  /** occurrences of this signature so far this session (1-based). */
  seen: number;
}

// Stay under internal/uitrace/uitrace.go's MaxBatchBytes (2 MiB) with
// headroom; flushes are chunked to this serialized-byte budget.
const MAX_BATCH_BYTES = 1024 * 1024;
const MAX_STACK_CHARS = 8 * 1024;
const MAX_MESSAGE_CHARS = 2 * 1024;
const MAX_SOURCE_CHARS = 2 * 1024;
const FLUSH_DELAY_MS = 1_000;
const MAX_PENDING_LINES = 100;
// Per-signature report cap. A throw inside a row that windowing remounts on
// every scroll repeats thousands of times per session; the first few
// occurrences carry all the signal. After the cap, only every 100th
// occurrence is recorded so sustained recurrence stays visible without
// burning the rotation budget.
const MAX_PER_SIGNATURE = 10;
const SUPPRESSED_SAMPLE_EVERY = 100;
// Messages that embed variable data (ids, URLs, timestamps) would mint a
// fresh signature per event — unbounded map growth AND a bypass of the
// per-signature cap. Past this many distinct signatures, new ones fold
// into a coarse per-throw-site bucket that still obeys the cap.
const MAX_DISTINCT_SIGNATURES = 1_000;
// A batch is re-queued on flush failure (transport blip during startup is
// exactly when mount-time errors happen). After this many consecutive
// failures the batch is dropped instead, so a server-side rejection of a
// poisoned batch can't retry forever.
const MAX_FLUSH_FAILURES = 3;

const utf8 = new TextEncoder();
const signatureCounts = new Map<string, number>();
const pendingLines: string[] = [];
let flushTimer: number | null = null;
let listenerAbort: AbortController | null = null;
let flushInFlight = false;
let consecutiveFlushFailures = 0;
// Set when the backend refuses the method (non-loopback remote client —
// ReportFrontendErrorBatch is host-scoped). Persisting is impossible for
// the rest of the session; stop capturing instead of warn-spamming.
let reporterUnavailable = false;

export function installFrontendErrorCapture(): void {
  if (listenerAbort !== null || typeof window === 'undefined') return;
  listenerAbort = new AbortController();
  const { signal } = listenerAbort;

  // Transport outage summaries (reconnect duration, close code, failed
  // attempts, watchdog force-closes) land in the same error log —
  // they're the after-the-fact evidence for "the UI stalled" reports.
  // Injected here rather than imported by the transport so wsClient
  // stays free of stores/bindings dependencies.
  wsClient.setDiagnosticsSink(reportFrontendDiagnostic);

  window.addEventListener('error', (event: ErrorEvent) => {
    // Synthetic or resource-flavored `error` events can arrive with no
    // payload at all; runtime exceptions always carry message or error.
    if (!event.message && !event.error) return;
    try {
      capture({
        kind: 'error',
        message: errorEventMessage(event),
        stack: stackOf(event.error),
        source: redact(truncate(event.filename ?? '', MAX_SOURCE_CHARS)),
        line: event.lineno ?? 0,
        col: event.colno ?? 0,
      });
    } catch (err) {
      // The reporter must never become an error source itself (a hostile
      // error object with throwing getters would otherwise loop straight
      // back into this listener).
      console.warn('[frontendErrorCapture] failed to record error event', err);
    }
  }, { signal });

  window.addEventListener('unhandledrejection', (event: PromiseRejectionEvent) => {
    try {
      capture({
        kind: 'unhandledrejection',
        message: messageOf(event.reason),
        stack: stackOf(event.reason),
        source: '',
        line: 0,
        col: 0,
      });
    } catch (err) {
      console.warn('[frontendErrorCapture] failed to record rejection', err);
    }
  }, { signal });

  // A Content-Security-Policy refusal is silent on every screen but the
  // devtools console: the image is missing, the script is inert. The e2e
  // fixture sees these only under Playwright's Chromium; in a native webview,
  // the phone shell and a --connect window this log is the one place a
  // refused load surfaces (spec §14). The event is document-level, unlike
  // the two above.
  document.addEventListener('securitypolicyviolation', (event: SecurityPolicyViolationEvent) => {
    try {
      capture({
        kind: 'csp',
        message: `${event.effectiveDirective || event.violatedDirective} refused ${redact(
          truncate(event.blockedURI || '(inline)', MAX_SOURCE_CHARS),
        )}`,
        stack: '',
        source: redact(truncate(event.sourceFile ?? '', MAX_SOURCE_CHARS)),
        line: event.lineNumber ?? 0,
        col: event.columnNumber ?? 0,
      });
    } catch (err) {
      console.warn('[frontendErrorCapture] failed to record CSP violation', err);
    }
  }, { signal });

  // Best-effort final flush: errors captured within FLUSH_DELAY_MS of the
  // window going away are often the crash being diagnosed.
  window.addEventListener('pagehide', () => {
    void flushFrontendErrors();
  }, { signal });
}

/**
 * Record an app-internal diagnostic through the same dedupe/cap/batch
 * pipeline as runtime errors, so a finding lands in the always-on error log
 * instead of a devtools console nobody is watching.
 *
 * `message` MUST be a constant and every variable — ids, counts, durations —
 * belongs in `detail`. The dedupe signature is built from the message, so an
 * interpolated id mints a fresh signature per occurrence: it walks straight
 * past `MAX_PER_SIGNATURE`, and it grows `signatureCounts` until the
 * distinct-signature overflow bucket catches it. Callers pair the report with
 * a `console.warn` carrying the same detail, because a non-loopback session
 * cannot persist at all (`ReportFrontendErrorBatch` is host-scoped) and the
 * console line is then the only surviving evidence.
 *
 * Current callers: the transport's outage/staleness summaries
 * (`transport/wsClient.ts`, injected as its diagnostics sink) and the
 * unbounded-loop guards — `utils/subagentGrouping.ts`,
 * `utils/sidebarTreeView.ts`, `utils/reentrantTrampoline.ts`,
 * `components/chat/timelineQuietWork.ts`, and
 * `stores/threadActivityRuns.svelte.ts`.
 */
export function reportFrontendDiagnostic(message: string, detail = ''): void {
  try {
    capture({
      kind: 'diagnostic',
      message: redact(truncate(message, MAX_MESSAGE_CHARS)),
      // Rides in the record's `stack` field: it is the record's free-form
      // slot, and it is deliberately outside the dedupe signature (only the
      // first `at …`/`…@…` frame of a real stack contributes), which is
      // exactly the property the constant-message rule needs.
      stack: redact(truncate(detail, MAX_STACK_CHARS)),
      source: '',
      line: 0,
      col: 0,
    });
  } catch (err) {
    console.warn('[frontendErrorCapture] failed to record diagnostic', err);
  }
}

function capture(record: Omit<CapturedErrorRecord, 'at' | 'seen'>): void {
  if (reporterUnavailable) return;

  let signature = `${record.kind}|${record.message}|${firstStackFrame(record.stack)}`;
  if (!signatureCounts.has(signature) && signatureCounts.size >= MAX_DISTINCT_SIGNATURES) {
    signature = `overflow|${record.kind}|${firstStackFrame(record.stack)}`;
  }
  const seen = (signatureCounts.get(signature) ?? 0) + 1;
  signatureCounts.set(signature, seen);
  if (seen > MAX_PER_SIGNATURE && seen % SUPPRESSED_SAMPLE_EVERY !== 0) return;

  const line = serializeRecord({ ...record, at: Date.now(), seen });
  if (!line) return;
  pendingLines.push(line);
  if (pendingLines.length > MAX_PENDING_LINES) {
    pendingLines.splice(0, pendingLines.length - MAX_PENDING_LINES);
  }
  scheduleFlush();
}

// WebKitGTK regularly reports uncaught exceptions with `error: null`
// while `message` carries the only useful text ("TypeError: ..."). A real
// Error object wins (it pairs with the captured stack); otherwise the
// event message; the stringified thrown value is the last resort so a
// thrown `null` can never shadow a real message (one session logged 800
// records of message:"null" exactly that way).
function errorEventMessage(event: ErrorEvent): string {
  if (event.error instanceof Error) {
    return messageOf(event.error) || redact(truncate(event.message ?? '', MAX_MESSAGE_CHARS));
  }
  return redact(truncate(event.message ?? '', MAX_MESSAGE_CHARS)) || messageOf(event.error);
}

function messageOf(value: unknown): string {
  if (value instanceof Error) return redact(truncate(value.message || value.name, MAX_MESSAGE_CHARS));
  if (typeof value === 'string') return redact(truncate(value, MAX_MESSAGE_CHARS));
  try {
    return redact(truncate(JSON.stringify(value) ?? String(value), MAX_MESSAGE_CHARS));
  } catch {
    return redact(truncate(String(value), MAX_MESSAGE_CHARS));
  }
}

function stackOf(value: unknown): string {
  if (value instanceof Error && value.stack) return redact(truncate(value.stack, MAX_STACK_CHARS));
  return '';
}

// Strip token-like query params before anything reaches disk. The app
// deliberately keeps transport tokens out of history and LAN responses;
// an error message embedding a full URL must not undo that.
function redact(value: string): string {
  return redactDiagnosticText(value);
}

function firstStackFrame(stack: string): string {
  // Frame 0 is usually the message line; the first `at`/`@` line anchors
  // the throw site for dedupe.
  for (const line of stack.split('\n').slice(0, 4)) {
    const trimmed = line.trim();
    if (trimmed.startsWith('at ') || trimmed.includes('@')) return trimmed;
  }
  return '';
}

function truncate(value: string, max: number): string {
  return value.length <= max ? value : `${value.slice(0, max)}…`;
}

function lineBytes(line: string): number {
  return utf8.encode(line).length;
}

function serializeRecord(record: CapturedErrorRecord): string | null {
  try {
    const line = JSON.stringify(record);
    if (lineBytes(line) <= UI_TRACE_MAX_LINE_BYTES) return line;
    // Field caps keep ordinary records far under the line cap; landing
    // here means something pathological (e.g. an exotic multi-byte blow
    // up). Keep the dedupe-relevant core and drop the bulk.
    const stripped = JSON.stringify({
      ...record,
      message: truncate(record.message, 512),
      stack: '',
      source: truncate(record.source, 256),
    });
    if (lineBytes(stripped) <= UI_TRACE_MAX_LINE_BYTES) return stripped;
    console.warn('[frontendErrorCapture] dropped oversized error record');
    return null;
  } catch (err) {
    console.warn('[frontendErrorCapture] failed to serialize error record', err);
    return null;
  }
}

function scheduleFlush(): void {
  if (flushTimer !== null || typeof window === 'undefined') return;
  flushTimer = window.setTimeout(() => {
    flushTimer = null;
    void flushFrontendErrors();
  }, FLUSH_DELAY_MS);
}

export async function flushFrontendErrors(): Promise<void> {
  if (flushInFlight || reporterUnavailable) return;
  flushInFlight = true;
  try {
    while (pendingLines.length > 0) {
      const batch = takeBatch();
      try {
        await ReportFrontendErrorBatch(batch);
        consecutiveFlushFailures = 0;
      } catch (err) {
        if (isMethodUnavailable(err)) {
          // Non-loopback client: the backend refuses local-FS writes from
          // this peer for the whole session. Warn once and stand down.
          reporterUnavailable = true;
          pendingLines.length = 0;
          console.warn(
            '[frontendErrorCapture] backend refuses error persistence for this connection; capture disabled',
          );
          return;
        }
        consecutiveFlushFailures += 1;
        if (consecutiveFlushFailures < MAX_FLUSH_FAILURES) {
          // Transient transport failure (reconnect window, startup blip):
          // put the batch back and retry on the next timer tick.
          pendingLines.unshift(...batch);
          if (pendingLines.length > MAX_PENDING_LINES) {
            pendingLines.splice(0, pendingLines.length - MAX_PENDING_LINES);
          }
          scheduleFlush();
        } else {
          consecutiveFlushFailures = 0;
          console.warn('[frontendErrorCapture] dropped error batch after repeated flush failures', err);
        }
        return;
      }
    }
  } finally {
    flushInFlight = false;
  }
}

// Take pending lines from the head up to the serialized-byte budget
// (always at least one — per-line caps keep any single line well under
// the budget).
function takeBatch(): string[] {
  let count = 0;
  let bytes = 0;
  while (count < pendingLines.length) {
    bytes += lineBytes(pendingLines[count]) + 1;
    if (bytes > MAX_BATCH_BYTES && count > 0) break;
    count += 1;
  }
  return pendingLines.splice(0, count);
}

function isMethodUnavailable(err: unknown): boolean {
  if (isMethodUnavailableError(err)) return true;
  // Message fallback, kept only for this sink: dropping error reporting is
  // cheap to get wrong in the tolerant direction and expensive in the strict
  // one (a refused ReportFrontendErrorBatch would otherwise retry forever
  // against a backend that will never accept it). Callers deciding user-facing
  // behaviour should use the code-only predicate above.
  if (!err || typeof err !== 'object') return false;
  const message = (err as { message?: unknown }).message;
  return typeof message === 'string' && message.includes('method not registered');
}

/** Test hook: detach listeners and reset module state between cases. */
export function resetFrontendErrorCaptureForTest(): void {
  wsClient.setDiagnosticsSink(null);
  signatureCounts.clear();
  pendingLines.length = 0;
  if (flushTimer !== null && typeof window !== 'undefined') {
    window.clearTimeout(flushTimer);
  }
  flushTimer = null;
  listenerAbort?.abort();
  listenerAbort = null;
  flushInFlight = false;
  consecutiveFlushFailures = 0;
  reporterUnavailable = false;
}

/** Test hook: bounded internals the tests assert against. */
export function frontendErrorCaptureStateForTest(): {
  distinctSignatures: number;
  pendingCount: number;
  reporterUnavailable: boolean;
} {
  return {
    distinctSignatures: signatureCounts.size,
    pendingCount: pendingLines.length,
    reporterUnavailable,
  };
}
