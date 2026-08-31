// The transport a call is routed through, resolved rather than imported.
//
// Every RPC and every event subscription in this app lands on ONE
// connection, and the layers that issue them reached the `wsClient`
// singleton by importing it. That is the assumption a second attached
// backend breaks, and it breaks it in every one of those layers at once,
// which is why the seam lands before the feature does
// (docs/specs/remote-access.md §10: "`bindings.ts` routes RPCs through a
// resolvable transport handle rather than importing a singleton").
//
// Today `resolveTransport()` has exactly one answer: the connection the
// bootstrap manifest named. When attaching to several backends lands, the
// resolution grows a target (and a registry to pick from) HERE, and the
// generated bindings, the runtime shim and the event hub above it do not
// change.
//
// Cost is one module-level call per RPC and no allocation. `origin` is
// rebuilt only when the backend identity moves, so a fan-out that stamps
// every event with it pays a string compare, never a new object.

import { getBackendIdentity } from './backendIdentity';
import { wsClient } from './wsClient';

/**
 * Which connection something arrived on. Carried by every event the hub
 * fans out, so a store that later has to tell two backends' events apart
 * reads a field instead of being re-plumbed.
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
  subscribe(channel: string, handler: (data: unknown) => void): () => void;
}

let origin: EventOrigin = { backendId: '' };

/** The one connection this client holds: the wsClient singleton. */
const attached: TransportHandle = {
  get origin(): EventOrigin {
    const backendId = getBackendIdentity().backendId;
    // Rebuilt only when the identity moves. Subscribers hold this object
    // for the life of their subscription and events stamp it by
    // reference, so a fresh object per event would be pure garbage.
    if (origin.backendId !== backendId) origin = { backendId };
    return origin;
  },
  callByID(methodId: number, args: unknown[]): Promise<unknown> {
    return wsClient.callByID(methodId, args);
  },
  callByName(method: string, args: unknown[]): Promise<unknown> {
    return wsClient.callByName(method, args);
  },
  subscribe(channel: string, handler: (data: unknown) => void): () => void {
    return wsClient.subscribe(channel, handler);
  },
};

/**
 * The transport to route through. Parameterless while one connection is
 * the only answer; the multi-backend form takes the backend to reach (or
 * enumerates the attached set for a fan-out) and every caller above this
 * keeps its shape.
 */
export function resolveTransport(): TransportHandle {
  return attached;
}
