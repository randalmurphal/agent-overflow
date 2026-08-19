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
// This module also owns the clipped-strip window: when the tick strip
// outgrows the rail column, the strip slides (translateY) so the
// reader's position claim stays centered in the window, and the
// first/latest jump arrows show only while their end tick is clipped
// out.
//
// Perf contract (the renderer-hang rules): callers invoke `schedule()`
// from the virtualizer's scroll callback, so everything here must stay
// rAF-coalesced binary-search math over engine-memoized offsets,
// written to the DOM directly (dataset flags, style.top, the strip's
// transform, arrow visibility) so a 60Hz scroll never touches Svelte
// reactivity. Writes are diff-only against (element, value) pairs, so
// unchanged values write nothing while a remounted element is always
// rewritten.

import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import {
  railClipOffsetPx,
  railGapLow,
  railMaxClipPx,
  tickFraction,
  tickNearestCenter,
  tickRangeInView,
  type MergedNavTicks,
} from './messageNavRail';

export interface NavRailSyncCtx {
  getListRef(): TimelineVirtualizerHandle | undefined;
  getTicks(): MergedNavTicks;
  getMarkerEl(): HTMLElement | undefined;
  /**
   * The tick strip — the full-height (count·spacing) layer holding the
   * ticks and the dot. When it outgrows the rail column the sync slides
   * it (translateY) so the reader's position stays windowed; undefined
   * or a strip that fits gets no writes.
   */
  getStripEl(): HTMLElement | undefined;
  /** The jump-to-first arrow: shown only while the FIRST tick is clipped out. */
  getFirstArrowEl(): HTMLElement | undefined;
  /** The jump-to-latest arrow: shown only while the LAST tick is clipped out. */
  getLatestArrowEl(): HTMLElement | undefined;
  /** The rail column's available height (0 before the first RO delivery). */
  getAvailableHeightPx(): number;
  /**
   * The y (viewport-relative px) of the visible band's center: top fade
   * inset + half the rail container's height. 0 (before the container's
   * first ResizeObserver delivery) falls back to the raw viewport
   * center.
   */
  getVisibleCenterY(): number;
  /** False suppresses the work entirely (rail hidden). */
  isEnabled(): boolean;
  /**
   * Fired (from the rAF, after the frame's writes) whenever the clip
   * offset actually changed. The component uses it to drop a held
   * hover: the strip can slide under a parked pointer on scrolls the
   * strip never sees (bottom-follow streaming, keyboard scroll, a
   * landing jump), and a preview anchored to the old clip would lie.
   */
  onClipChange?(): void;
}

export interface NavRailTickRegistration {
  update(next: number): void;
  destroy(): void;
}

export interface NavRailViewportSync {
  /** Svelte action body: keeps the element array index-aligned with the ticks. */
  registerTick(el: HTMLElement, index: number): NavRailTickRegistration;
  /**
   * The strip's current slide offset (px), for pointer→tick mapping and
   * preview anchoring. A plain read of the last synced value — never
   * reactive, callers re-read per interaction.
   */
  getClipOffsetPx(): number;
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
  // Applied caches keyed on the ELEMENT the value was written to, which
  // is what makes the diff-only writes remount-proof: a rebuilt element
  // (the {#if overflowing} arrows remount born hidden; the marker and
  // strip remount on a rail visibility toggle) arrives with default
  // styles, and a value-equal skip against the OLD element's state
  // would strand it. The element identity mismatch forces the write.
  let markerEl: HTMLElement | null = null;
  let markerTop = '';
  let stripEl: HTMLElement | null = null;
  let stripClip = 0;
  let firstArrowEl: HTMLElement | null = null;
  let firstArrowOn = false;
  let latestArrowEl: HTMLElement | null = null;
  let latestArrowOn = false;
  // The last COMPUTED clip, reported through getClipOffsetPx. Distinct
  // from stripClip (the applied cache) and never reset: between a
  // structural pass and its rAF the mounted strip still holds the old
  // translate, so this staying put is what keeps pointer mapping and
  // the preview anchor agreeing with the DOM.
  let clipPx = 0;
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
    const next =
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
              );
    applyCurrent(next);

    // The reader's position as a strip fraction (0 = first message,
    // 1 = latest) plus the gap dot's fraction. Both resolve from the
    // ONE claim above: a current tick IS the position (and hides the
    // dot); mid-gap the shared gap search answers both.
    const count = merged.ticks.length;
    let positionFrac: number;
    let markerFrac: number | null = null;
    if (next !== null) {
      positionFrac = tickFraction(next, count);
    } else {
      const gapLo = railGapLow(merged, center, (nodeIndex) => list.getItemOffset(nodeIndex));
      if (gapLo === null) {
        // No loaded geometry to measure against: fall back to the raw
        // scroll proportion so the clipped strip still tracks.
        positionFrac = maxOffset > 0 ? Math.min(Math.max(offset / maxOffset, 0), 1) : 1;
      } else if (gapLo < 0) {
        positionFrac = 0;
      } else if (gapLo >= count - 1) {
        positionFrac = 1;
      } else {
        markerFrac = (gapLo + 0.5) / (count - 1);
        positionFrac = markerFrac;
      }
    }

    const marker = ctx.getMarkerEl();
    if (marker) {
      // One claim at a time (module header): a lit current tick hides
      // the dot; mid-gap the dot is centered between the two ticks the
      // reader is between, hidden at the ends (null). Hiding is the
      // visibility style so the element (and its bound ref) stays.
      const top = markerFrac === null ? '' : `${markerFrac * 100}%`;
      if (marker !== markerEl || top !== markerTop) {
        markerEl = marker;
        markerTop = top;
        marker.style.visibility = markerFrac === null ? 'hidden' : '';
        if (markerFrac !== null) marker.style.top = top;
      }
    }

    // Clipped-strip slide + end arrows. availableH 0 (pre-RO, or a
    // column squeezed below the reserve) renders nothing measurable —
    // the arrows are unmounted in exactly that state — so skip rather
    // than write garbage geometry; the element-keyed caches make the
    // remount on regrow rewrite everything.
    const availableH = ctx.getAvailableHeightPx();
    if (availableH <= 0) return;
    const clip = railClipOffsetPx(positionFrac, count, availableH);
    const clipMoved = clip !== clipPx;
    clipPx = clip;
    const strip = ctx.getStripEl();
    if (strip && (strip !== stripEl || clip !== stripClip)) {
      stripEl = strip;
      stripClip = clip;
      strip.style.transform = clip > 0 ? `translateY(${-clip}px)` : '';
    }
    // Each arrow shows only while its end tick has slid out of the
    // window proper (the clip wrapper's few px of grace may still show
    // a sliver of it) — at the strip's own end the tick is in the
    // window and the arrow yields. Half a px of slack absorbs rounding.
    const maxClip = railMaxClipPx(count, availableH);
    const firstClipped = clip > 0.5;
    const latestClipped = maxClip - clip > 0.5;
    const firstArrow = ctx.getFirstArrowEl() ?? null;
    if (firstArrow && (firstArrow !== firstArrowEl || firstClipped !== firstArrowOn)) {
      firstArrowEl = firstArrow;
      firstArrowOn = firstClipped;
      firstArrow.style.visibility = firstClipped ? '' : 'hidden';
    }
    const latestArrow = ctx.getLatestArrowEl() ?? null;
    if (latestArrow && (latestArrow !== latestArrowEl || latestClipped !== latestArrowOn)) {
      latestArrowEl = latestArrow;
      latestArrowOn = latestClipped;
      latestArrow.style.visibility = latestClipped ? '' : 'hidden';
    }
    // After the writes, so a listener reading getClipOffsetPx() or the
    // DOM sees the settled frame.
    if (clipMoved) ctx.onClipChange?.();
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
    getClipOffsetPx() {
      return clipPx;
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
