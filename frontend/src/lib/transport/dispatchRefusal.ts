// The refusals a computer's dispatcher gives before any method runs: a call
// it does not have (`method_not_found`) and a call whose arguments it cannot
// read (`bad_params`). Neither ran, and the page and the computer were often
// built from different releases.
//
// Who refused is never inferred from the code. A current dispatcher marks
// its own refusal with the frame's `dispatch` field (internal/
// dispatchrefusal's Refusal); a failure a method returned never carries it,
// whatever its code. A frame without the field comes from a release before
// it (v0.0.14 to v0.0.19) and is that release's dispatcher speaking only
// when its whole message is one of `LEGACY_DISPATCH_REFUSALS`. Such a
// release forwarding another computer's dispatcher refusal is recognized
// by the same texts inside its forwarding sentence (`forwardedRefusalKind`),
// and keeps its message with advice added after it. Anything else is
// shown as it was sent.
//
// Every RPC refusal becomes a `TransportError` (./wsClient.ts). For a
// dispatcher refusal it captures the refusing computer's facts when the
// frame is accepted (`captureRefusingComputer`), and its message is the
// sentence `dispatchRefusalMessage` builds from them when the message is
// first read. What did not fit, or the older release's own text, stays as
// the error's `cause`, which Details and a copied report list.
//
// The two sentences here are complete on their own, so an entry point,
// worker or test that never installs a wording still says what happened.
// They are the Go side's sentences, which a current computer sends as the
// refusal's message, and the legacy table is the Go side's, both pinned by
// internal/dispatchrefusal's tests. The app installs a wording
// (stores/versionMismatch.ts) that names the computer and says what the
// page knows about the versions.

import type { BackendKey } from './backendKey';

export const UNSUPPORTED_SENTENCE =
  'The computer this reached does not support this action. It may run a different version of Agent Overflow. If so, update the older one, or reload a page left open during an update.';
export const UNREADABLE_SENTENCE =
  'The computer this reached could not read this request. It may run a different version of Agent Overflow. If so, update the older one, or reload a page left open during an update.';

/** `unsupported`: no such call there. `unreadable`: its arguments did not fit. */
export type DispatchRefusalKind = 'unsupported' | 'unreadable';

/** The frame's `dispatch` field, as read. */
export interface DispatchMark {
  readonly kind: DispatchRefusalKind;
  /** The method, for an unreadable refusal. */
  readonly method?: string;
  /** The 1-based argument that did not decode; absent for a wrong count. */
  readonly param?: number;
  /** What did not fit, from the dispatcher's closed set of shapes. */
  readonly problem?: string;
}

const MAX_METHOD = 128;
const MAX_PROBLEM = 160;

/** The `dispatch` field of a frame, or undefined when it is absent or malformed. */
export function parseDispatchMark(raw: unknown): DispatchMark | undefined {
  if (raw === null || typeof raw !== 'object') return undefined;
  const { refusal, method, param, problem } = raw as Record<string, unknown>;
  if (refusal !== 'unsupported' && refusal !== 'unreadable') return undefined;
  if (refusal === 'unsupported') return { kind: 'unsupported' };
  return {
    kind: 'unreadable',
    method: typeof method === 'string' && method.length > 0 && method.length <= MAX_METHOD ? method : undefined,
    param: typeof param === 'number' && Number.isInteger(param) && param > 0 && param <= 64 ? param : undefined,
    problem: typeof problem === 'string' && problem.length > 0 && problem.length <= MAX_PROBLEM ? problem : undefined,
  };
}

/** What did not fit, as internal/dispatchrefusal's Diagnostic says it, or ''. */
export function dispatchDiagnostic(mark: DispatchMark): string {
  if (mark.kind !== 'unreadable' || mark.method === undefined) return '';
  const what = mark.param === undefined ? mark.method : `${mark.method} argument ${mark.param}`;
  return mark.problem === undefined ? `${what} could not be read` : `${what}: ${mark.problem}`;
}

/**
 * Every text a dispatcher of v0.0.14 to v0.0.19 sent before a method ran,
 * each the whole message; no method of those releases sent one as its own.
 * The same table as internal/dispatchrefusal's `legacy`, pinned there.
 */
export const LEGACY_DISPATCH_REFUSALS: readonly { code: string; kind: DispatchRefusalKind; text: RegExp }[] = [
  // The dispatcher's missing or hidden method, and scopedtoken.go's
  // unlisted method, in every release from v0.0.14 to v0.0.19.
  { code: 'method_not_found', kind: 'unsupported', text: /^method not registered$/ },
  { code: 'bad_params', kind: 'unreadable', text: /^too many parameters$/ },
  { code: 'bad_params', kind: 'unreadable', text: /^[A-Za-z0-9_]+\.[A-Za-z0-9_]+\.[A-Za-z0-9_]+: expected \d+ params, got \d+$/ },
  { code: 'bad_params', kind: 'unreadable', text: /^[A-Za-z0-9_]+\.[A-Za-z0-9_]+\.[A-Za-z0-9_]+: variadic expects at least \d+ params, got \d+$/ },
  // v0.0.14 to v0.0.16 with no detail, v0.0.17 to v0.0.19 with detail.
  { code: 'bad_params', kind: 'unreadable', text: /^bad parameter \d+$/ },
];

/**
 * Which dispatcher refusal a wire error is: its `dispatch` mark, else an
 * older release's exact text, else null for a method's own failure.
 */
export function dispatchRefusalKind(code: unknown, message: string, mark: DispatchMark | undefined): DispatchRefusalKind | null {
  if (mark !== undefined) return mark.kind;
  for (const entry of LEGACY_DISPATCH_REFUSALS) {
    if (entry.code === code && entry.text.test(message)) return entry.kind;
  }
  return null;
}

// How v0.0.15 to v0.0.19 forward a downstream refusal they cannot word
// (internal/app/app_remote_errors.go's remoteErrorDetails, default case),
// after a prefix that names the job and the computer.
const FORWARDED_LEAD = ': The destination could not complete the request. Its reported error: ';
const FORWARDED_TAIL = '. If it is running an older AO build, update it for detailed command errors.';

/**
 * Which dispatcher refusal a computer on v0.0.15 to v0.0.19 forwarded from
 * the computer it called, recognized by an older release's exact text
 * inside its forwarding sentence, or null.
 */
export function forwardedRefusalKind(code: unknown, message: string, mark: DispatchMark | undefined): DispatchRefusalKind | null {
  if (mark !== undefined || !message.endsWith(FORWARDED_TAIL)) return null;
  const lead = message.lastIndexOf(FORWARDED_LEAD);
  if (lead <= 0) return null;
  const reported = message.slice(lead + FORWARDED_LEAD.length, message.length - FORWARDED_TAIL.length);
  return dispatchRefusalKind(code, reported, undefined);
}

/** What follows a forwarded refusal from an older release (`forwardedRefusalKind`). */
export const FORWARDED_ADVICE =
  'This can happen when the computers involved run different versions of Agent Overflow. If so, update the older one.';

/** The computer that refused, as it was when its refusal arrived. */
export interface RefusingComputer {
  readonly backend: BackendKey;
  /** Its name then, or null when it was no longer attached. */
  readonly name: string | null;
  /** The release its connection's hello reported then, or ''. */
  readonly version: string;
}

/**
 * The app's wording (stores/versionMismatch.ts). `capture` reads the
 * refusing computer's facts when its frame is accepted; `word` builds the
 * sentence from them when the message is first read, or answers null for
 * the sentence above. Both are synchronous and free of I/O.
 */
export interface DispatchRefusalWording {
  capture(backend: BackendKey): RefusingComputer;
  word(kind: DispatchRefusalKind, computer: RefusingComputer): string | null;
}

/** Where a wording failure is reported (utils/frontendErrorCapture.ts). */
export type DispatchRefusalDiagnosticsSink = (message: string, detail: string) => void;

let wording: DispatchRefusalWording | null = null;
let diagnosticsSink: DispatchRefusalDiagnosticsSink | null = null;

/** Installed once at boot by `src/main.ts`; null uninstalls (tests). */
export function installDispatchRefusalWording(next: DispatchRefusalWording | null): void {
  wording = next;
}

export function setDispatchRefusalDiagnosticsSink(sink: DispatchRefusalDiagnosticsSink | null): void {
  diagnosticsSink = sink;
}

/**
 * The refusing computer's facts now, for the frame just accepted from
 * `backend`. Never throws: a capture that fails is reported and leaves no
 * name, which words the refusal with the sentences above.
 */
export function captureRefusingComputer(backend: BackendKey): RefusingComputer {
  if (wording !== null) {
    try {
      return wording.capture(backend);
    } catch (err) {
      report(err);
    }
  }
  return { backend, name: null, version: '' };
}

/**
 * What a person reads for a refusal of `kind` by `computer`. Never throws:
 * a wording that fails is reported and the sentence above stands in for it.
 */
export function dispatchRefusalMessage(kind: DispatchRefusalKind, computer: RefusingComputer): string {
  const fallback = kind === 'unsupported' ? UNSUPPORTED_SENTENCE : UNREADABLE_SENTENCE;
  if (wording === null) return fallback;
  try {
    return wording.word(kind, computer) || fallback;
  } catch (err) {
    report(err);
    return fallback;
  }
}

// A wording failure goes to the diagnostics sink, and to the console when
// the sink is missing or fails too.
function report(err: unknown): void {
  try {
    if (diagnosticsSink === null) throw new Error('no diagnostics sink is installed');
    diagnosticsSink('transport: dispatch refusal wording failed', err instanceof Error ? err.message : String(err));
  } catch (sinkErr) {
    console.error('transport: dispatch refusal wording failed', err, sinkErr);
  }
}
