// PaneHost's promote/demote controller: moves the strip toward the layout
// store's promoted pane (`getPromotedPaneId`) with a clip reveal.
//
// A promoted pane's section stays the same element in the same place in
// the DOM. While shown it is absolutely positioned over the strip's visible
// area (the host's padding box at the locked scroll position), and a
// placeholder with the section's flex style takes its slot, so no other
// pane moves and the strip's widths and `scrollLeft` are untouched. The
// strip does not scroll while a pane is shown; `holdStrip` puts back any
// scroll that happens anyway.
//
// No transform: the timeline virtualizer reads bounding-rect differences
// as scroll compensation. Promote applies the layout in one pass, with a
// `clip-path: inset()` equal to the slot, then opens the clip to the full
// area. Demote closes the clip back to the slot (measured from the
// placeholder) and then restores the layout in one pass. So the pane body
// sees one size change per promote and one per demote. The clip is
// JS-driven so its rect is known each frame and published to native
// browser views (utils/panePromotion.svelte.ts).
//
// Transitions settle deterministically: a demote during a promote, or a
// promote of the same pane during its demote, reverses from the current
// clip; a different pane waits for the demote to land and then promotes;
// a shown pane that leaves the layout ends at once; reduced motion and
// compact land in the same task.

import type { AirspaceRect } from '../../utils/paneAirspace.svelte';
import { setPromotedLayer } from '../../utils/panePromotion.svelte';
import { motionReduced } from '../../utils/reducedMotion';

export const PANE_PROMOTE_DURATION_MS = 180;
// A stalled frame advances at most this much, so a resumed tab does not
// finish the reveal in one jump it never showed.
const MAX_FRAME_MS = 40;

interface Inset {
  top: number;
  right: number;
  bottom: number;
  left: number;
}

const NO_INSET: Inset = { top: 0, right: 0, bottom: 0, left: 0 };

export interface PanePromotionOptions {
  getHostEl(): HTMLElement | undefined;
  /** A pane's section element (`data-pane-id`), or null. */
  sectionOf(paneId: string): HTMLElement | null;
  /** A pane's slot placeholder (`data-pane-slot`) while it is shown. */
  slotOf(paneId: string): HTMLElement | null;
  /** A pane is about to be shown: the strip stops moving. */
  onCapture(): void;
  /** No pane is shown any more. */
  onRelease(): void;
}

export interface FollowOptions {
  /** Land without animating (compact). */
  instant: boolean;
  /** Whether a pane id is still in the layout. */
  present(paneId: string): boolean;
}

const clamp = (value: number, min: number, max: number): number => Math.max(min, Math.min(max, value));

function easeInOutCubic(t: number): number {
  return t < 0.5 ? 4 * t * t * t : 1 - (-2 * t + 2) ** 3 / 2;
}

/** The host's padding box in viewport px: where a shown pane sits. */
function hostBox(host: HTMLElement): AirspaceRect {
  const rect = host.getBoundingClientRect();
  const left = rect.left + host.clientLeft;
  const top = rect.top + host.clientTop;
  return { left, top, right: left + host.clientWidth, bottom: top + host.clientHeight };
}

// The inset that clips the full box down to the slot's visible part. A
// slot partly scrolled out of view clips to what of it shows.
function slotInset(slot: DOMRect, box: AirspaceRect): Inset {
  const width = box.right - box.left;
  const height = box.bottom - box.top;
  const left = clamp(slot.left - box.left, 0, width);
  const top = clamp(slot.top - box.top, 0, height);
  return {
    left,
    top,
    right: clamp(box.right - slot.right, 0, width - left),
    bottom: clamp(box.bottom - slot.bottom, 0, height - top),
  };
}

export function createPanePromotion(options: PanePromotionOptions) {
  // The pane laid out as promoted, from the first frame of its promote to
  // the last of its demote.
  let shownId = $state<string | null>(null);
  // The strip's scrollLeft while a pane is shown; the shown section's left.
  let lockedLeft = $state(0);
  let clipPath = $state<string | null>(null);
  // The store's intent, as last followed.
  let target: string | null = null;
  // 0: clipped to the slot; 1: fully revealed.
  let progress = 0;
  let direction: -1 | 0 | 1 = 0;
  let inset: Inset = NO_INSET;
  let frame = 0;
  let lastTs: number | null = null;

  function cancelFrame(): void {
    if (frame) cancelAnimationFrame(frame);
    frame = 0;
    lastTs = null;
  }

  // Writes the current clip to the section and publishes it.
  function apply(): void {
    const id = shownId;
    const section = id === null ? null : options.sectionOf(id);
    const host = options.getHostEl();
    if (!section || !host) return;
    if (direction === 0 && progress === 1) {
      clipPath = null;
      setPromotedLayer(section, null);
      return;
    }
    const k = 1 - easeInOutCubic(progress);
    const top = inset.top * k;
    const right = inset.right * k;
    const bottom = inset.bottom * k;
    const left = inset.left * k;
    clipPath = `inset(${top}px ${right}px ${bottom}px ${left}px)`;
    const box = hostBox(host);
    setPromotedLayer(section, {
      left: box.left + left,
      top: box.top + top,
      right: box.right - right,
      bottom: box.bottom - bottom,
    });
  }

  function release(): void {
    cancelFrame();
    direction = 0;
    progress = 0;
    shownId = null;
    clipPath = null;
    setPromotedLayer(null, null);
  }

  // A demote landed: the layout returns to the slot in the same flush.
  function finishDemote(): void {
    release();
    if (target !== null) startPromote(target);
    else options.onRelease();
  }

  // Ends the transition once progress reaches its end; answers whether it
  // did.
  function land(): boolean {
    if (direction === 1 && progress >= 1) {
      progress = 1;
      direction = 0;
      cancelFrame();
      apply();
      return true;
    }
    if (direction === -1 && progress <= 0) {
      finishDemote();
      return true;
    }
    return false;
  }

  function step(ts: number): void {
    frame = 0;
    const dt = lastTs === null ? 16 : clamp(ts - lastTs, 0, MAX_FRAME_MS);
    lastTs = ts;
    progress = clamp(progress + (direction * dt) / PANE_PROMOTE_DURATION_MS, 0, 1);
    if (land()) return;
    apply();
    frame = requestAnimationFrame(step);
  }

  function run(): void {
    if (motionReduced()) {
      progress = direction === 1 ? 1 : 0;
      land();
      return;
    }
    if (!frame) {
      lastTs = null;
      frame = requestAnimationFrame(step);
    }
  }

  function startPromote(id: string): void {
    const host = options.getHostEl();
    const section = options.sectionOf(id);
    if (!host || !section) return;
    // Measured in flow, before the layout changes: this is the slot.
    inset = slotInset(section.getBoundingClientRect(), hostBox(host));
    lockedLeft = host.scrollLeft;
    options.onCapture();
    shownId = id;
    progress = 0;
    direction = 1;
    apply();
    run();
  }

  function startDemote(): void {
    if (direction !== -1) {
      const host = options.getHostEl();
      const slot = shownId === null ? null : options.slotOf(shownId);
      // The slot can have changed while shown (a window resize). Before the
      // placeholder first renders, the promote's measurement still holds.
      if (host && slot) inset = slotInset(slot.getBoundingClientRect(), hostBox(host));
      direction = -1;
    }
    run();
  }

  function settleNow(next: string | null): void {
    cancelFrame();
    if (next === null) {
      if (shownId !== null) release();
      options.onRelease();
      return;
    }
    const host = options.getHostEl();
    const section = options.sectionOf(next);
    if (!host || !section) return;
    if (shownId !== next) {
      lockedLeft = host.scrollLeft;
      options.onCapture();
    }
    shownId = next;
    progress = 1;
    direction = 0;
    apply();
  }

  /** Move toward the store's promoted pane. Call untracked. */
  function follow(next: string | null, opts: FollowOptions): void {
    target = next;
    // A shown pane that left the layout has no section to animate.
    if (shownId !== null && !opts.present(shownId)) {
      release();
      if (next === null || !opts.present(next)) {
        options.onRelease();
        return;
      }
    }
    if (opts.instant) {
      settleNow(next);
      return;
    }
    if (shownId === null) {
      if (next !== null) startPromote(next);
      return;
    }
    if (next === shownId) {
      if (direction === -1) {
        direction = 1;
        run();
      }
      return;
    }
    startDemote();
  }

  /**
   * The strip moved while a pane is shown (scroll, resize clamp): put it
   * back, and keep the shown section over the visible area if it cannot go
   * back. `maxLeft` is the strip's scroll range without the shown section,
   * which as an absolute box widens the range to wherever it sits: after a
   * resize the lock follows the clamp the strip takes once demoted.
   */
  function holdStrip(host: HTMLElement, maxLeft = Number.POSITIVE_INFINITY): void {
    if (shownId === null) return;
    if (lockedLeft > maxLeft) lockedLeft = Math.max(0, maxLeft);
    if (Math.abs(host.scrollLeft - lockedLeft) > 0.5) host.scrollLeft = lockedLeft;
    if (Math.abs(host.scrollLeft - lockedLeft) > 0.5) lockedLeft = host.scrollLeft;
  }

  function destroy(): void {
    cancelFrame();
    if (shownId !== null) setPromotedLayer(null, null);
  }

  return {
    get shownId() {
      return shownId;
    },
    get lockedLeft() {
      return lockedLeft;
    },
    /** The clip-path for a pane's section; undefined when it has none. */
    clipFor(paneId: string): string | undefined {
      return shownId === paneId && clipPath !== null ? clipPath : undefined;
    },
    follow,
    holdStrip,
    destroy,
  };
}

export type PanePromotion = ReturnType<typeof createPanePromotion>;
