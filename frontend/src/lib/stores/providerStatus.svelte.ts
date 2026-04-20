import type { ProviderStatusEvent } from '../types/events';
import { wailsEventOn } from './events';

export type { ProviderStatusEvent } from '../types/events';

/**
 * Per-provider snapshot of the most recent status event. We keep the
 * entry even when the status is "ready" so consumers can observe the
 * clear-banner signal; the component treats ready as "render nothing".
 *
 * Reassigned wholesale on every update rather than mutated in place.
 * Svelte 5's $state proxy tracks assignment to the binding, not Map
 * mutations; writing `statuses = new Map(statuses).set(...)` is the
 * established pattern elsewhere in the codebase (threadStatuses.svelte.ts)
 * and is what makes $derived consumers re-run.
 */
let statuses: Map<ProviderStatusEvent['provider'], ProviderStatusEvent> = $state(new Map());

/**
 * Read the latest status for a given provider, or null if no event
 * has been received yet. Components call this in a $derived.by and
 * re-evaluate whenever the pane's provider flips.
 */
export function getProviderStatus(
  provider: ProviderStatusEvent['provider'],
): ProviderStatusEvent | null {
  return statuses.get(provider) ?? null;
}

/**
 * setupProviderStatusListener subscribes to the Wails `provider:status`
 * channel. Returns a cleanup function so the caller (App.svelte) can
 * unsubscribe on teardown. Events arrive wrapped in SeqEnvelope — the
 * wailsEventOn helper unwraps the `data` field automatically and tracks
 * per-channel seq gaps for us.
 *
 * Idempotent: re-emitting the same state twice is safe; the map just
 * replaces the entry in place.
 */
export function setupProviderStatusListener(): () => void {
  return wailsEventOn<ProviderStatusEvent>('provider:status', (evt) => {
    if (!evt || typeof evt.provider !== 'string' || typeof evt.status !== 'string') {
      // Malformed payloads are dropped silently; wailsEventOn already
      // logs the raw event at the gap-detection layer when something
      // is off. A bad payload shouldn't blow up the whole bus.
      return;
    }
    statuses = new Map(statuses).set(evt.provider, evt);
  });
}

/**
 * Wipes the in-memory map. Called from tests between cases so one test's
 * state doesn't leak into the next. Not exported for production code —
 * the map is app-lifetime in real use.
 */
export function resetForTest(): void {
  if (statuses.size === 0) return;
  statuses = new Map();
}
