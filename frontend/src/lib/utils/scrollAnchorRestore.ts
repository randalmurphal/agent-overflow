// Anchor capture + restore helpers for the chat timeline. Extracted out
// of MessageTimeline.svelte so the component owns rendering and the
// snapshot/anchor pipeline lives where it can be exercised in isolation.
//
// The flow:
//   1. Before a layout-changing operation (load-older, thread-switch),
//      call `firstVisibleItemAnchor(container)` to capture the first
//      item that's at-or-below the viewport top, plus its offset.
//   2. After the operation, call `restoreAnchorSnapshot` (async — pages
//      history back via `loadUntilItem` until the captured item is in
//      the DOM) or `restoreLoadedAnchorSnapshot` (sync-load case — item
//      is already in the rendered window) to put the user back on the
//      same row at the same on-screen pixel.
//
// Both restore functions take `shouldContinue` so the caller can cancel
// stale work via a token (load-older raced by thread-switch, etc.) and
// roll back any partial scroll write before bailing.

import { tick } from 'svelte';
import type { ScrollSnapshot } from './threadScrollSnapshots';

export type AnchorSnapshot = Extract<ScrollSnapshot, { kind: 'anchor' }>;

/**
 * Find the first item element at or below the container's viewport top
 * and return its id + offset relative to the viewport. Used both by
 * the snapshot-save path (capture before thread switch) and by the
 * live-anchor effect (capture before timeline-revision-driven re-render).
 */
export function firstVisibleItemAnchor(container: HTMLElement): ScrollSnapshot | null {
  const viewport = container.getBoundingClientRect();
  const items = Array.from(container.querySelectorAll<HTMLElement>('[data-item-id]'));
  for (const el of items) {
    const rect = el.getBoundingClientRect();
    if (rect.height <= 0) continue;
    if (rect.bottom < viewport.top) continue;
    const itemId = el.dataset.itemId ?? '';
    if (!itemId) continue;
    return {
      kind: 'anchor',
      itemId,
      offsetTop: rect.top - viewport.top,
    };
  }
  return null;
}

export type RestoreLoadedAnchorOptions = {
  container: HTMLElement;
  snapshot: AnchorSnapshot;
  /** Locate the index of the timeline node containing the snapshot's item. */
  findNodeIndex: (itemId: string) => number;
  /** Map a node index to its virtual-layout pixel offset. */
  offsetForIndex: (index: number) => number;
  /** Re-sync the component's viewport-derived $state after a scroll write. */
  syncViewportState: () => void;
  /**
   * Cancel guard: returning false aborts the restore mid-flight. Any
   * already-applied scroll write is rolled back to `previousScrollTop`
   * before bailing, provided no further scroll has happened since.
   */
  shouldContinue?: () => boolean;
};

/**
 * Restore an anchor snapshot when the target item is already loaded into
 * the rendered window. Two-pass: first scroll to the virtual offset
 * (best-effort given estimated row heights), then await `tick` and
 * adjust by the actual rect delta.
 */
export async function restoreLoadedAnchorSnapshot(
  opts: RestoreLoadedAnchorOptions,
): Promise<boolean> {
  const shouldContinue = opts.shouldContinue ?? (() => true);
  const { container, snapshot } = opts;
  if (!snapshot.itemId) return false;
  await tick();
  if (!shouldContinue()) return false;

  const targetIndex = opts.findNodeIndex(snapshot.itemId);
  if (targetIndex < 0) return false;

  const previousScrollTop = container.scrollTop;
  container.scrollTop = Math.max(0, opts.offsetForIndex(targetIndex) - snapshot.offsetTop);
  const approximatedScrollTop = container.scrollTop;
  opts.syncViewportState();
  await tick();
  if (!shouldContinue()) {
    if (container.scrollTop === approximatedScrollTop) {
      container.scrollTop = previousScrollTop;
      opts.syncViewportState();
    }
    return false;
  }

  const el = container.querySelector(`[data-item-id="${CSS.escape(snapshot.itemId)}"]`);
  if (!(el instanceof HTMLElement)) {
    if (container.scrollTop === approximatedScrollTop) {
      container.scrollTop = previousScrollTop;
      opts.syncViewportState();
    }
    return false;
  }

  const viewport = container.getBoundingClientRect();
  const rect = el.getBoundingClientRect();
  if (!shouldContinue()) {
    if (container.scrollTop === approximatedScrollTop) {
      container.scrollTop = previousScrollTop;
      opts.syncViewportState();
    }
    return false;
  }
  container.scrollTop += rect.top - viewport.top - snapshot.offsetTop;
  opts.syncViewportState();
  return true;
}

export type RestoreAnchorOptions = RestoreLoadedAnchorOptions & {
  /** Page history back until the snapshot's item is in the loaded window. */
  loadUntilItem: (id: string) => Promise<boolean>;
};

/**
 * Restore an anchor snapshot for an item that may live below the loaded
 * floor. Pages back via `loadUntilItem` first, then delegates to
 * `restoreLoadedAnchorSnapshot`.
 */
export async function restoreAnchorSnapshot(
  opts: RestoreAnchorOptions,
): Promise<boolean> {
  const shouldContinue = opts.shouldContinue ?? (() => true);
  if (!opts.snapshot.itemId || !shouldContinue()) return false;
  const found = await opts.loadUntilItem(opts.snapshot.itemId);
  if (!found || !shouldContinue()) return false;
  return restoreLoadedAnchorSnapshot(opts);
}
