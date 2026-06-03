// Shared typed wrapper over the Wails runtime event bus.
//
// This lives in its own leaf module — it imports only `@wailsio/runtime` — so
// any store can subscribe to a backend-emitted event without importing the
// heavy `events.ts` handler module. Importing `events.ts` from a low-level
// store (e.g. gitStatus.svelte) would re-form the `events → thread.svelte →
// gitStatus → events` ESM cycle that previously corrupted `events.ts`
// init-order and silently dropped its handler registration. Subscribing
// through this leaf keeps those stores decoupled from the event graph.
//
// `events.ts` re-exports `wailsEventOn` so existing `./events` import sites keep
// their path; new low-level subscribers should import it from here directly.
import { Events } from '@wailsio/runtime';

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
 */
export function wailsEventOn<T = unknown>(
  name: string,
  handler: (data: T) => void,
): () => void {
  return Events.On(name, (ev) => handler(ev.data as T));
}
