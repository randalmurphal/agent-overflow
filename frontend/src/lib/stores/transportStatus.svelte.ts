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
  type TransportHello,
  type TransportStatusSnapshot,
} from '../transport/wsClient';

let snapshot = $state<TransportStatusSnapshot>({
  status: 'disconnected',
  nextAttemptAt: null,
});

type TransportStatusListener = (snapshot: TransportStatusSnapshot) => void;

const edgeListeners = new Set<TransportStatusListener>();

function publish(next: TransportStatusSnapshot): void {
  snapshot = next;
  for (const listener of edgeListeners) listener(next);
}

let unsubscribe: (() => void) | null = wsClient.onStatusChange(publish);

export function getTransportStatus(): TransportStatusSnapshot {
  return snapshot;
}

// The hello frame's contents, mirrored into runes for the same reason as
// the status snapshot: it changes on a connection edge, and consumers
// render from it. Subscribed at module load so a $derived can read it
// without mutating state inside a derive.
let hello = $state<TransportHello | null>(null);

let unsubscribeHello: (() => void) | null = wsClient.onHelloChange((next) => {
  hello = next;
});

/** What the attached backend said about itself, or null before any hello
 *  has arrived. Reactive; safe to read from a `$derived`. */
export function getTransportHello(): TransportHello | null {
  return hello;
}

/**
 * Whether the attached backend advertises `capability`.
 *
 * The one compatibility question feature code is allowed to ask. No
 * hello and an unrecognised name both answer false, so a feature
 * degrades rather than being attempted against a backend that cannot
 * serve it. Reactive, unlike `wsClient.hasCapability`, so a UI written
 * against it re-evaluates when a reconnect lands on a backend with a
 * different answer.
 *
 * There is deliberately no protocol-version accessor here: gating on a
 * version guesses at what a number implies, gating on a flag asks
 * (docs/specs/remote-access.md §9). A flag is also never authorization —
 * the backend re-checks every RPC regardless.
 */
export function backendHasCapability(capability: string): boolean {
  return hello?.capabilities.includes(capability) ?? false;
}

/**
 * Subscribe to connection-state changes imperatively. Fires once
 * immediately with the current snapshot, then on every change.
 *
 * For RENDERING, read `getTransportStatus()` from a `$derived` — that is the
 * reactive surface. This is for stores that must act on the EDGE: a
 * reconnect silently invalidates every subscription the backend was holding
 * for the old socket, so the owning store has to re-acquire them. An
 * `$effect` would work but needs an owning root and only fires a microtask
 * later, which is pure overhead for a listener that renders nothing.
 */
export function onTransportStatusChange(listener: TransportStatusListener): () => void {
  listener(snapshot);
  edgeListeners.add(listener);
  return () => {
    edgeListeners.delete(listener);
  };
}

/**
 * Test seam: drive the connection snapshot without a live socket, through
 * the same publish path the wsClient uses so edge listeners see it.
 *
 * The unit suite has no transport, so the module-load snapshot is
 * `disconnected` — which is not the state any test means to exercise.
 * `src/test/setup.ts` pins it to `connected` before each test; tests that
 * care about an outage drive it themselves.
 */
export function __setTransportStatusForTest(next: TransportStatusSnapshot): void {
  publish(next);
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

/** Whether the backend completed the RPC with a safe-to-retry transient error. */
export function isTemporarilyUnavailableError(err: unknown): boolean {
  return err instanceof TransportError && err.code === 'temporarily_unavailable';
}

/**
 * Whether an RPC rejection means "this backend exposes no such method to
 * this caller".
 *
 * One wire shape covers two causes, deliberately: a genuinely
 * unregistered method, and a method on a host-tooling receiver (the
 * harness's, registered `RegisterOptions{LocalOnly: true}`) refused to a
 * non-loopback peer. `internal/transport/dispatcher.go` returns the
 * identical `method_not_found` envelope for both so a scanner cannot
 * fingerprint which receivers exist. Callers therefore read it as "this
 * capability isn't available on this session", never as a failure to
 * report.
 *
 * It is NOT how an ordinary App method is refused for want of a grant:
 * that is `scope_required`, which names the missing capability and is
 * presented by ./transport/scopeRefusal.ts. A surface that wants to
 * degrade quietly on either reads both.
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

/**
 * Re-attach after this browser acquired a session it did not have — a
 * passkey sign-in from the terminal-refusal banner, the same way the
 * pairing screen re-attaches after redemption.
 *
 * Deliberately the SAME call rather than a second recovery path: the
 * socket that is up (or latched) predates the credential and would carry
 * the wrong identity, and the awaited settle is what stops the boot
 * fan-out being issued against a transport mid-transition
 * (transport/AGENTS.md § redialAfterPairing is AWAITED).
 */
export function redialAfterSignIn(): Promise<void> {
  return wsClient.redialAfterPairing();
}

/** Test-only helper. Tears down the subscription so a subsequent test
 * can re-seed the singleton from a fresh wsClient. */
export function resetTransportStatusForTest(): void {
  if (unsubscribe) {
    unsubscribe();
    unsubscribe = null;
  }
  if (unsubscribeHello) {
    unsubscribeHello();
    unsubscribeHello = null;
  }
  snapshot = { status: 'disconnected', nextAttemptAt: null };
  hello = null;
}

/**
 * Test seam: drive the hello snapshot without a live socket. The unit
 * suite has no transport, so nothing would ever populate it; a test that
 * exercises a capability-gated path sets what it needs.
 */
export function __setTransportHelloForTest(next: TransportHello | null): void {
  hello = next;
}
