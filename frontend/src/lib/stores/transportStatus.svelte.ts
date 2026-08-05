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

import { wsClient, type TransportStatusSnapshot } from '../transport/wsClient';

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
