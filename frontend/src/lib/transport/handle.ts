// The transport a call is routed through, resolved rather than imported.
//
// Every RPC and every event subscription in this app lands on a
// connection, and the layers that issue them used to reach the `wsClient`
// singleton by importing it. That is the assumption a second attached
// backend breaks, and it breaks it in every one of those layers at once,
// which is why the seam landed before the feature did
// (docs/specs/remote-access.md §10: "`bindings.ts` routes RPCs through a
// resolvable transport handle rather than importing a singleton").
//
// Phase 7 gives the seam its parameter. `resolveTransport(backendId)`
// answers the handle for one attached backend; omitting the argument
// answers the page's own — which is what every existing call site means
// and why none of them had to change. The registry behind it is
// ./backends.ts, and the layers above (the generated bindings, the
// runtime shim, the event hub) still do not know it exists.
//
// Cost is one Map lookup per RPC and no allocation. `origin` is rebuilt
// only when a backend's identity moves, so a fan-out that stamps every
// event with it pays a string compare, never a new object.

import { HOME_BACKEND, type BackendKey } from './backendKey';
import { backendById } from './backends';
import type { LeaseState } from './frames';
import type { StepUpProver } from './wsClient';

/**
 * Which connection something arrived on. Carried by every event the hub
 * fans out, so a store that has to tell two backends' events apart reads
 * a field instead of being re-plumbed.
 *
 * `backendId` is empty when the backend does not identify itself (the
 * `--connect` stub, an older server). Empty means UNKNOWN, never "any" —
 * the same rule backendIdentity.ts states for the identity itself.
 */
export interface EventOrigin {
  readonly backendId: string;
}

/** What a transport must provide to carry this app's RPCs and events. */
export interface TransportHandle {
  /** Identity to stamp on events arriving over this connection. */
  readonly origin: EventOrigin;
  callByID(methodId: number, args: unknown[]): Promise<unknown>;
  callByName(method: string, args: unknown[]): Promise<unknown>;
  /**
   * Install the seam that satisfies a step-up refusal for every call
   * routed over this connection. ./stepUp.ts fills it — through
   * ./backends.ts, which installs on every attached handle AND on every
   * later attach — and ./wsClient.ts owns what happens under it.
   */
  installStepUpProver(prover: StepUpProver | null): void;
  /**
   * State this client's foreground lifecycle on this connection. Whole
   * client, so ./backends.ts fans it out to every attached backend and
   * ./lease.ts is the single door — a per-handle call is the mechanism, not
   * the interface a caller reaches for.
   */
  setLease(state: LeaseState): void;
  /**
   * Narrow this connection's entity-filtered channels to `threadIds`.
   *
   * Per connection because that is what the frame does: a machine can
   * only push frames about threads it holds. ./backends.ts owns the SPLIT
   * (`setWatchedThreadsEverywhere`) and `stores/watchedThreads.ts` owns
   * the composition; a per-handle call is the mechanism, not the
   * interface a caller reaches for.
   */
  setWatchedThreads(threadIds: readonly string[]): void;
  /**
   * State whether the screen behind this connection is being looked at,
   * and which threads it shows.
   *
   * Neither of the two above: it narrows nothing and sheds no work. The
   * backend reads it for one decision — whether to raise an OS
   * notification about something already on screen — and only for its own
   * machine's connections. ./backends.ts fans it out
   * (`setPresenceEverywhere`) and `stores/screenPresence.ts` composes it.
   */
  setPresence(focused: boolean, threadIds: readonly string[]): void;
  subscribe(channel: string, handler: (data: unknown) => void): () => void;
}

/**
 * The transport to route through.
 *
 * Omitted means the home connection. An explicit unknown target is an
 * error: a disappeared computer must never turn a path-based mutation into
 * an action against another computer's filesystem.
 *
 * The argument accepts either spelling of a backend — its registry id or
 * its live UUID off an event's origin stamp — because both are in the
 * registry's index.
 */
export function resolveTransport(backendId: BackendKey = HOME_BACKEND): TransportHandle {
  const backend = backendById(backendId);
  if (!backend) throw new Error('The selected computer is no longer connected. Choose a computer and try again.');
  return backend.handle;
}
