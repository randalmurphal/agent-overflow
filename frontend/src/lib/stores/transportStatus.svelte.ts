// transportStatus is a runes-backed mirror of the wsClient's connection
// state. We expose the snapshot as $state so the App-level connection
// banner re-renders when the transport flips between connected /
// reconnecting / disconnected without each consumer re-implementing the
// onStatusChange subscription.
//
// The store subscribes at module load so the first $derived consumer
// can read the snapshot without triggering a state mutation inside the
// derive. Subscribing lazily from getTransportStatus would re-mutate
// `snapshot` synchronously on first read (the wsClient calls the
// handler immediately with the seeded snapshot) and that's forbidden
// inside a derive.

import {
  DisconnectedError,
  TransportError,
  wsClient,
  type TransportStatusSnapshot,
} from '../transport/wsClient';

let snapshot = $state<TransportStatusSnapshot>({
  status: 'disconnected',
  nextAttemptAt: null,
});

let unsubscribe: (() => void) | null = wsClient.onStatusChange((next) => {
  snapshot = next;
});

export function getTransportStatus(): TransportStatusSnapshot {
  return snapshot;
}

/**
 * Whether an RPC rejection means "the wire broke", as opposed to "the
 * backend answered and refused".
 *
 * The distinction matters to any caller whose RPC has side effects: a
 * refusal is a definite "nothing happened", while a transport-class
 * failure leaves the caller epistemically crash-equivalent — the request
 * may have been fully executed on the far side and only its answer lost.
 * A timed-out RPC is in the same class for a stronger reason: the server
 * is still holding it, so it can complete AFTER the rejection.
 *
 * Feature code classifies through this predicate rather than importing
 * the transport's error classes, which stay behind `stores/` per
 * `frontend/AGENTS.md`.
 */
export function isTransportClassError(err: unknown): boolean {
  return err instanceof DisconnectedError
    || (err instanceof TransportError && err.code === 'timeout');
}

/**
 * Whether an RPC rejection means "this backend exposes no such method to
 * this caller".
 *
 * One wire shape covers two causes, deliberately: a genuinely
 * unregistered method, and a `LocalOnlyMethods` method refused to a
 * non-loopback peer. `internal/transport/dispatcher.go` returns the
 * identical `method_not_found` envelope for both so a LAN scanner cannot
 * fingerprint which methods are privileged. Callers therefore read it as
 * "this capability isn't available on this session", never as a failure
 * to report.
 *
 * The discriminator is the wire error CODE (`ErrCodeMethodNotFound` in
 * `internal/transport/frame.go`), never message prose. Every other
 * failure carries a different code — a method that returned an error is
 * `method_error`, a decode failure is `bad_params`, an RPC timeout is
 * `timeout`, and a dead socket rejects with `DisconnectedError` (no code
 * at all) — so this cannot swallow a real backend or network failure.
 *
 * Duck-typed on `code` rather than `instanceof TransportError` so a
 * rejection that crossed a serialization boundary still classifies.
 *
 * Feature code classifies through this predicate rather than importing
 * the transport's error classes, which stay behind `stores/` per
 * `frontend/AGENTS.md`.
 */
export function isMethodUnavailableError(err: unknown): boolean {
  return typeof err === 'object'
    && err !== null
    && (err as { code?: unknown }).code === 'method_not_found';
}

/**
 * Resolves the next time the transport is connected — immediately when it
 * already is. One-shot: the subscription is dropped as soon as it fires,
 * so a caller awaiting it cannot leak a handler across reconnect cycles.
 *
 * Never rejects. A transport that never comes back leaves the promise
 * pending forever, which is the honest answer for "do this once we can
 * talk to the backend again" — the caller has nothing to do meanwhile.
 */
export function whenTransportConnected(): Promise<void> {
  return new Promise<void>((resolve) => {
    let unsubscribe: (() => void) | null = null;
    let settled = false;
    unsubscribe = wsClient.onStatusChange((next) => {
      if (settled || next.status !== 'connected') return;
      settled = true;
      // `onStatusChange` invokes the handler synchronously with the
      // current snapshot, so on an already-connected transport this runs
      // BEFORE the assignment above completes — the post-call check below
      // is what drops the subscription in that case.
      unsubscribe?.();
      resolve();
    });
    if (settled) unsubscribe();
  });
}

/** Force a reconnect attempt immediately. Wired to the banner's "Retry"
 * button; safe to call when already connected (no-op). This is also the
 * only way out of the terminal 'unauthorized' state — it un-latches the
 * client for one user-initiated attempt — so keep it reachable from any
 * surface that renders that state. */
export function retryTransport(): void {
  wsClient.triggerReconnect();
}

/** Test-only helper. Tears down the subscription so a subsequent test
 * can re-seed the singleton from a fresh wsClient. */
export function resetTransportStatusForTest(): void {
  if (unsubscribe) {
    unsubscribe();
    unsubscribe = null;
  }
  snapshot = { status: 'disconnected', nextAttemptAt: null };
}
