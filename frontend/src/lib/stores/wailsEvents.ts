// Shared typed wrapper over the Wails runtime event bus.
//
// This lives in its own leaf module — at runtime it imports only
// `@wailsio/runtime`, the type import below being erased — so any store can
// subscribe to a backend-emitted event without importing the heavy
// `events.ts` handler module. Importing `events.ts` from a low-level
// store (e.g. gitStatusStore.svelte) would re-form the `events →
// thread.svelte → gitStatusStore → events` ESM cycle that previously
// corrupted `events.ts` init-order and silently dropped its handler
// registration. Subscribing through this leaf keeps those stores decoupled
// from the event graph.
//
// `events.ts` re-exports `wailsEventOn` so existing `./events` import sites keep
// their path; new low-level subscribers should import it from here directly.
import { Events } from '@wailsio/runtime';
// Type-only, so the leaf stays a leaf: the transport handle that defines
// EventOrigin imports the WS client, and this module must not.
import type { EventOrigin } from '../transport/handle';

/**
 * Origin handed to a subscriber when the runtime delivered an event
 * without one. Stated as unknown rather than assumed to be the attached
 * backend: a client that ends up holding two connections must not read a
 * missing stamp as "mine".
 */
const UNKNOWN_EVENT_ORIGIN: EventOrigin = { backendId: '' };

/**
 * Subscribes to a backend-emitted Wails event and returns an unsubscribe fn.
 *
 * Unwraps the runtime envelope so the handler receives the inner Go payload
 * directly. The transport (wsClient.ts) is payload-agnostic and already hands
 * `ev.data` through as the bare payload, so this is purely an unwrap + a single
 * import path for subscribers.
 *
 * Per-channel gap detection lives in the transport, not here: the wsClient
 * surfaces gaps via the synthetic `transport:gap` channel and the `gap:true`
 * flag on `event` frames. Subscribers that care about gap recovery consume that
 * channel directly rather than re-implementing seq tracking.
 *
 * The handler's second argument is the connection the event arrived on
 * (docs/specs/remote-access.md §10, "event fan-out carries connection
 * origin"). Every subscriber today serves one backend and ignores it;
 * the stores that will have to tell two backends apart read it instead of
 * being re-plumbed when a second connection attaches. Passed as an
 * argument rather than folded into the payload so no channel's shape
 * changes and nothing has to be unwrapped.
 */
export function wailsEventOn<T = unknown>(
  name: string,
  handler: (data: T, origin: EventOrigin) => void,
): () => void {
  return Events.On(name, (ev) => handler(ev.data as T, ev.origin ?? UNKNOWN_EVENT_ORIGIN));
}
