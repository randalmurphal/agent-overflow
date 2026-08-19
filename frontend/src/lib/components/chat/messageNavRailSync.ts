// The nav rail's imperative viewport-sync half: which single tick is
// the accent-filled "current" message and where the position dot sits.
// Extracted from MessageNavRail.svelte in the timeline sibling-module
// shape.
//
// The rail makes exactly ONE position claim at a time. Any user message
// on screen → the one whose row is closest to the visible-band center
// lights, and the dot hides; the dot appears only mid-gap, when no user
// message is visible at all. "Visible-band center" is the center of
// what the reader actually sees — the viewport minus the composer
// overlay and top fade — supplied by the component from the rail
// container's own extent (it spans exactly that band), because the raw
// scroll viewport extends under the composer.
//
// Perf contract (the renderer-hang rules): callers invoke `schedule()`
// from the virtualizer's scroll callback, so everything here must stay
// rAF-coalesced binary-search math over engine-memoized offsets,
// written to the DOM directly (dataset flags + one style.top) so a
// 60Hz scroll never touches Svelte reactivity. Writes are diff-only:
// an unchanged current tick and an unchanged marker position write
// nothing.

import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import {
  markerGapFraction,
  tickNearestCenter,
  tickRangeInView,
  type MergedNavTicks,
} from './messageNavRail';

export interface NavRailSyncCtx {
  getListRef(): TimelineVirtualizerHandle | undefined;
  getTicks(): MergedNavTicks;
  getMarkerEl(): HTMLElement | undefined;
  /**
   * The y (viewport-relative px) of the visible band's center: top fade
   * inset + half the rail container's height. 0 (before the container's
   * first ResizeObserver delivery) falls back to the raw viewport
   * center.
   */
  getVisibleCenterY(): number;
  /** False suppresses the work entirely (rail hidden). */
  isEnabled(): boolean;
}

export interface NavRailTickRegistration {
  update(next: number): void;
  destroy(): void;
}

export interface NavRailViewportSync {
  /** Svelte action body: keeps the element array index-aligned with the ticks. */
  registerTick(el: HTMLElement, index: number): NavRailTickRegistration;
  /** rAF-coalesced sync; the public scroll-path entry. */
  schedule(): void;
  /**
   * Structural pass: the keyed tick list reuses surviving elements, so
   * the applied current flag is cleared by hand (indices shifted under
   * it) before the resync recomputes.
   */
  reset(): void;
  /** Teardown: drop the pending frame. */
  cancel(): void;
}

// Scroll-edge overrides: within this many px of the scroller's hard
// edge, AND only when that edge is the THREAD's (no unloaded ticks
// beyond it — mid-history the loaded window's edge is a transient
// paging seam, not a place), the edge-most on-screen tick lights
// regardless of center distance. At a hard edge the reader is AT the
// first/last message even though another may sit closer to the visible
// center: the scroller cannot go further, so the edge message can never
// be centered.
const AT_EDGE_EPSILON_PX = 2;

export function createNavRailViewportSync(ctx: NavRailSyncCtx): NavRailViewportSync {
  // Plain array of tick elements, index-aligned with the tick list.
  // Deliberately not reactive: the scroll-cadence writer mutates
  // dataset directly.
  const tickEls: (HTMLElement | null)[] = [];
  let appliedCurrent: number | null = null;
  let lastMarkerTop = '';
  let frame: number | undefined;

  function applyCurrent(next: number | null): void {
    if (appliedCurrent === next) return;
    if (appliedCurrent !== null) {
      const prev = tickEls[appliedCurrent];
      if (prev) prev.dataset.current = 'false';
    }
    if (next !== null) {
      const el = tickEls[next];
      if (el) el.dataset.current = 'true';
    }
    appliedCurrent = next;
  }

  function syncNow(): void {
    const list = ctx.getListRef();
    if (!list) return;
    const merged = ctx.getTicks();
    if (merged.ticks.length === 0) {
      applyCurrent(null);
      return;
    }
    const viewport = list.getViewportSize();
    if (viewport <= 0) return;
    const offset = list.getScrollOffset();
    const first = list.findItemIndex(offset);
    const last = list.findItemIndex(offset + viewport - 1);
    const range = tickRangeInView(merged, first, last);
    const centerY = ctx.getVisibleCenterY();
    const center = offset + (centerY > 0 ? centerY : viewport / 2);
    // Edge overrides only apply to a scrollable thread: when everything
    // fits one screen there is no scroll position, both "edges" hold at
    // once, and nearest-to-center is the only non-arbitrary answer.
    const maxOffset = list.getTotalSize() - viewport;
    const scrollable = maxOffset > AT_EDGE_EPSILON_PX;
    const atThreadTop =
      scrollable && offset <= AT_EDGE_EPSILON_PX && merged.loadedStart === 0;
    const atThreadBottom =
      scrollable &&
      offset >= maxOffset - AT_EDGE_EPSILON_PX &&
      merged.loadedEnd === merged.ticks.length - 1;
    applyCurrent(
      range === null
        ? null
        : atThreadTop
          ? range[0]
          : atThreadBottom
            ? range[1]
            : tickNearestCenter(
                merged,
                range,
                center,
                (nodeIndex) => list.getItemOffset(nodeIndex),
                (nodeIndex) => list.sizeAt(nodeIndex),
              ),
    );
    const marker = ctx.getMarkerEl();
    if (marker) {
      // One claim at a time (module header): a lit current tick hides
      // the dot; mid-gap the dot is centered between the two ticks the
      // reader is between, hidden at the ends (null). Hiding is the
      // visibility style so the element (and its bound ref) stays.
      const frac =
        range !== null
          ? null
          : markerGapFraction(merged, center, (nodeIndex) => list.getItemOffset(nodeIndex));
      const top = frac === null ? '' : `${frac * 100}%`;
      if (top !== lastMarkerTop) {
        lastMarkerTop = top;
        marker.style.visibility = frac === null ? 'hidden' : '';
        if (frac !== null) marker.style.top = top;
      }
    }
  }

  function schedule(): void {
    if (frame !== undefined || !ctx.isEnabled()) return;
    frame = requestAnimationFrame(() => {
      frame = undefined;
      syncNow();
    });
  }

  return {
    registerTick(el, index) {
      tickEls[index] = el;
      return {
        update(next) {
          if (next !== index) {
            if (tickEls[index] === el) tickEls[index] = null;
            index = next;
            tickEls[index] = el;
          }
        },
        destroy() {
          if (tickEls[index] === el) tickEls[index] = null;
        },
      };
    },
    schedule,
    reset() {
      for (const el of tickEls) {
        if (el) el.dataset.current = 'false';
      }
      appliedCurrent = null;
      schedule();
    },
    cancel() {
      if (frame !== undefined) {
        cancelAnimationFrame(frame);
        frame = undefined;
      }
    },
  };
}
