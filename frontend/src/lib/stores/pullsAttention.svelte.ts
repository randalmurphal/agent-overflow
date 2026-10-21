// The Pull requests tool's "needs you" count for the sidebar's tools row
// (docs/specs/pull-requests.md, "Tools row"): MRs in Your turn, Pipeline
// failed and Ready to merge on the selected computer. One count per
// computer: `pulls:badge` frames carry the pump's count as it changes,
// each connection edge reads `GetPRListNeedsYou` (the persisted list's
// count, right before the page is ever opened), and the page's own
// snapshots set it too, so the badge and the page never disagree. A
// computer that detaches takes its count.

import { SvelteMap } from 'svelte/reactivity';
import { GetPRListNeedsYou } from './bindings';
import { selectedBackend } from './selectedBackend.svelte';
import type { BackendKey } from '../transport/backendKey';
import { onBackendDetached, withBackendTarget } from '../transport/backends';
import { hasScope } from '../transport/scopes';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';

const counts = new SvelteMap<BackendKey, number>();
let detachInstalled = false;

function installDetach(): void {
  if (detachInstalled) return;
  detachInstalled = true;
  onBackendDetached((detached) => {
    counts.delete(detached.backendId);
  });
}

/** Reactive: the selected computer's count; zero until it is known. */
export function getPullsNeedsYouCount(): number {
  return counts.get(selectedBackend()) ?? 0;
}

export function setPullsNeedsYouCount(backend: BackendKey, count: number): void {
  installDetach();
  counts.set(backend, Number.isInteger(count) && count > 0 ? count : 0);
}

/** A `pulls:badge` frame from `backend`. */
export function applyPullsBadge(frame: { needsYou?: number } | null | undefined, backend: BackendKey): void {
  if (!frame || typeof frame.needsYou !== 'number') return;
  setPullsNeedsYouCount(backend, frame.needsYou);
}

/** The connection-edge read: the computer's persisted count. A computer
 * without the list's scope, or one that cannot answer, keeps the count
 * it had. */
export async function hydratePullsAttention(backend: BackendKey): Promise<void> {
  if (!hasScope('git:operate', backend)) return;
  try {
    const count = await withBackendTarget(backend, () => GetPRListNeedsYou());
    setPullsNeedsYouCount(backend, Number(count));
  } catch (err) {
    if (!isPassiveConnectionFailure(err)) console.warn('pulls: reading the needs-you count failed:', err);
  }
}

export function resetPullsAttentionForTest(): void {
  counts.clear();
}
