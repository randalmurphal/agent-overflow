// The refusals a computer's dispatcher gives before any method runs: a call
// it does not have (`method_not_found`) and a call whose arguments it cannot
// read (`bad_params`). Neither ran, and both usually mean the page and the
// computer were built from different releases. A dispatcher refusal carries
// no diagnostic detail; a failure a method returned always does
// (internal/transport's CallerError), which is what tells them apart when a
// method forwards another computer's refusal under the same code.
//
// Every RPC refusal becomes a `TransportError` (./wsClient.ts). For a
// dispatcher refusal its message is the sentence `dispatchRefusalMessage`
// gives, built when the message is first read, and the wire text stays as
// the error's `cause`, which Details and a copied report list.
//
// The two sentences here are complete on their own, so an entry point,
// worker or test that never installs a wording still says what happened.
// They are the Go side's sentences (internal/dispatchrefusal), which a
// current computer sends as the refusal's message, pinned to these by that
// package's tests. The app installs a wording (stores/versionMismatch.ts)
// that names the computer and says what the page knows about the versions.

import type { BackendKey } from './backendKey';

export const UNSUPPORTED_SENTENCE =
  'The computer this reached does not support this action. It may run a different version of Agent Overflow. If so, update the older one, or reload a page left open during an update.';
export const UNREADABLE_SENTENCE =
  'The computer this reached could not read this request. It may run a different version of Agent Overflow. If so, update the older one, or reload a page left open during an update.';

/** `unsupported`: no such call there. `unreadable`: its arguments did not fit. */
export type DispatchRefusalKind = 'unsupported' | 'unreadable';

/**
 * Which dispatcher refusal a wire error is, or null for any other failure.
 * `detail` is the error's parsed diagnostic detail.
 */
export function dispatchRefusalKind(code: unknown, detail: unknown): DispatchRefusalKind | null {
  if (detail !== undefined && detail !== null) return null;
  if (code === 'method_not_found') return 'unsupported';
  if (code === 'bad_params') return 'unreadable';
  return null;
}

/**
 * The sentence for a refusal of `kind` from `backend`, or null for the
 * sentence above. Synchronous and free of I/O.
 */
export type DispatchRefusalWording = (kind: DispatchRefusalKind, backend: BackendKey) => string | null;

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
 * What a person reads for a refusal of `kind` from `backend`. Never throws:
 * a wording that fails is reported and the sentence above stands in for it,
 * and a sink that fails too goes to the console.
 */
export function dispatchRefusalMessage(kind: DispatchRefusalKind, backend: BackendKey): string {
  const fallback = kind === 'unsupported' ? UNSUPPORTED_SENTENCE : UNREADABLE_SENTENCE;
  if (wording === null) return fallback;
  try {
    return wording(kind, backend) || fallback;
  } catch (err) {
    try {
      if (diagnosticsSink === null) throw new Error('no diagnostics sink is installed');
      diagnosticsSink('transport: dispatch refusal wording failed', err instanceof Error ? err.message : String(err));
    } catch (sinkErr) {
      console.error('transport: dispatch refusal wording failed', err, sinkErr);
    }
    return fallback;
  }
}
