import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { onBackendDetached } from '../transport/backends';
import { compositeKey } from '../utils/compositeKey';
import type { ProviderStatusEvent } from '../types/events';

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
let statuses: Map<string, ProviderStatusEvent> = $state(new Map());

/**
 * Read the latest status for a given provider, or null if no event
 * has been received yet. Components call this in a $derived.by and
 * re-evaluate whenever the pane's provider flips.
 */
export function getProviderStatus(
  provider: ProviderStatusEvent['provider'],
  backend: BackendKey = HOME_BACKEND,
): ProviderStatusEvent | null {
  return statuses.get(compositeKey(backend, provider)) ?? null;
}

/**
 * recordProviderStatus updates the app-wide per-provider status map.
 * Accepts either the legacy binary-detect shape (`status` populated) or
 * the chat-rewrite kind-bearing shape (`kind` populated, `status`
 * derived at the event boundary). Events lacking both a provider AND
 * at least one of `status`/`kind` are dropped as malformed.
 *
 * Called from the consolidated `provider:status` listener in
 * `eventsProvider.ts applyProviderStatus`; kept as the sole mutator on
 * the map so the store has one entry point.
 */
export function recordProviderStatus(evt: ProviderStatusEvent | null | undefined, backend: BackendKey = HOME_BACKEND): void {
  if (!evt || typeof evt.provider !== 'string') return;
  const hasStatus = typeof evt.status === 'string' && evt.status.length > 0;
  const hasKind = typeof evt.kind === 'string' && evt.kind.length > 0;
  if (!hasStatus && !hasKind) return;
  statuses = new Map(statuses).set(compositeKey(backend, evt.provider), evt);
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

onBackendDetached(({ backendId }) => {
  const prefix = compositeKey(backendId, '');
  statuses = new Map([...statuses].filter(([key]) => !key.startsWith(prefix)));
});
