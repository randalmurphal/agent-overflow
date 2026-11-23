// A computer answers a call it does not have with `method_not_found`
// (internal/transport/dispatcher.go): it runs a different release than the
// page that called it. Every RPC refusal becomes a `TransportError`
// (./wsClient.ts), whose constructor takes its message for this code from
// `methodUnavailableMessage` and keeps the wire text as the error's `cause`.
// So every surface that shows an error reads this sentence, and Details and
// a copied report still carry the code and the wire text.
//
// The sentence here is complete on its own: an entry point, worker or test
// that never installs the wording still says what happened. The app installs
// a wording (stores/versionMismatch.ts) that names the computer, says which
// side is older and what to do; it lives in stores because the computer's
// display name does. The Go side's sentence for the same refusal is
// `rpcclient.MethodUnavailableMessage`, pinned to this text by
// internal/rpcclient's tests.

import type { BackendKey } from './backendKey';

export const METHOD_UNAVAILABLE_SENTENCE =
  'The computer this reached runs a different version of Agent Overflow and does not support this. Update Agent Overflow so both run the same version.';

/**
 * The sentence for a `method_not_found` from `backend`, or null for the
 * sentence above. Synchronous and free of I/O: it runs inside the
 * constructor of every such error.
 */
export type MethodUnavailableWording = (backend: BackendKey) => string | null;

/** Where a wording failure is reported (utils/frontendErrorCapture.ts). */
export type MethodUnavailableDiagnosticsSink = (message: string, detail: string) => void;

let wording: MethodUnavailableWording | null = null;
let diagnosticsSink: MethodUnavailableDiagnosticsSink | null = null;

/** Installed once at boot by `src/main.ts`; null uninstalls (tests). */
export function installMethodUnavailableWording(next: MethodUnavailableWording | null): void {
  wording = next;
}

export function setMethodUnavailableDiagnosticsSink(sink: MethodUnavailableDiagnosticsSink | null): void {
  diagnosticsSink = sink;
}

/**
 * What a person reads when `backend` refused a call it does not have.
 * Never throws: a wording that fails is reported and the sentence above
 * stands in for it.
 */
export function methodUnavailableMessage(backend: BackendKey): string {
  if (wording === null) return METHOD_UNAVAILABLE_SENTENCE;
  try {
    return wording(backend) || METHOD_UNAVAILABLE_SENTENCE;
  } catch (err) {
    const detail = err instanceof Error ? err.message : String(err);
    if (diagnosticsSink !== null) diagnosticsSink('transport: version mismatch wording failed', detail);
    else console.error('transport: version mismatch wording failed', err);
    return METHOD_UNAVAILABLE_SENTENCE;
  }
}
