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
// Both ends are measured live every frame: the host's box and the slot
// (the placeholder, or before it renders the section's rect in flow). When
// either moves mid-transition (a window resize), the clip retargets
// without a jump: the difference between the clip last shown and the new
// path at that clip's progress is carried as a correction that shrinks to
// nothing as the transition ends, so it lands exactly on the new slot or
// box. Rebasing at the last shown progress keeps the step this frame
// advanced, so a resize on every frame still converges.
//
// Transitions settle deterministically: a demote during a promote, or a
// promote of the same pane during its demote, reverses from the current
// clip; a different pane waits for the demote to land and then promotes;
// a shown pane that leaves the layout ends at once; reduced motion and
// compact land in the same task.

import type { AirspaceRect } from '../../utils/paneAirspace.svelte';
import { setPromotedLayer } from '../../utils/panePromotion.svelte';
import { motionReduced } from '../../utils/reducedMotion';

const PANE_PROMOTE_DURATION_MS = 180;
// A stalled frame advances at most this much, so a resumed tab does not
// finish the reveal in one jump it never showed.
const MAX_FRAME_MS = 40;

const NO_OFFSET: AirspaceRect = { left: 0, top: 0, right: 0, bottom: 0 };

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

// A rect's visible part inside the box: a slot partly scrolled out of view
// clips to what of it shows.
function within(rect: AirspaceRect, box: AirspaceRect): AirspaceRect {
  const left = clamp(rect.left, box.left, box.right);
  const top = clamp(rect.top, box.top, box.bottom);
  return {
    left,
    top,
    right: clamp(rect.right, left, box.right),
    bottom: clamp(rect.bottom, top, box.bottom),
  };
}

function plainRect(rect: DOMRect): AirspaceRect {
  return { left: rect.left, top: rect.top, right: rect.right, bottom: rect.bottom };
}

function rectKey(rect: AirspaceRect): string {
  return `${rect.left},${rect.top},${rect.right},${rect.bottom}`;
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
  // The slot as measured in flow when the promote started, until the
  // placeholder renders.
  let startSlot: AirspaceRect | null = null;
  // Retargeting: the geometry the path was last computed for, the clip last
  // shown, and the correction carried from a retarget with the progress left
  // when it was taken.
  let geometryKey = '';
  let lastClip: AirspaceRect | null = null;
  let correction: AirspaceRect = NO_OFFSET;
  let correctionBase = 0;
  // The progress the last shown clip was drawn at.
  let lastProgress = 0;
  let frame = 0;
  let lastTs: number | null = null;

  function cancelFrame(): void {
    if (frame) cancelAnimationFrame(frame);
    frame = 0;
    lastTs = null;
  }

  // Eased distance left before the current direction lands: the correction
  // shrinks with it, so it is gone as smoothly as the path arrives.
  function remaining(at = progress): number {
    const e = easeInOutCubic(at);
    if (direction === 1) return 1 - e;
    if (direction === -1) return e;
    return 0;
  }

  function correctionWeight(): number {
    return correctionBase > 0 ? clamp(remaining() / correctionBase, 0, 1) : 0;
  }

  function resetPath(): void {
    geometryKey = '';
    lastClip = null;
    correction = NO_OFFSET;
    correctionBase = 0;
  }

  // Turns the transition around: the correction keeps its current size and
  // shrinks over what is left in the new direction.
  function turn(next: -1 | 1): void {
    const w = correctionWeight();
    correction = {
      left: correction.left * w,
      top: correction.top * w,
      right: correction.right * w,
      bottom: correction.bottom * w,
    };
    direction = next;
    correctionBase = remaining();
  }

  // Writes the current clip to the section and publishes it.
  function apply(): void {
    const id = shownId;
    const section = id === null ? null : options.sectionOf(id);
    const host = options.getHostEl();
    if (!section || !host) return;
    if (direction === 0 && progress === 1) {
      clipPath = null;
      resetPath();
      setPromotedLayer(section, null);
      return;
    }
    const box = hostBox(host);
    const slotEl = options.slotOf(id as string);
    const measured = slotEl ? plainRect(slotEl.getBoundingClientRect()) : startSlot;
    const slot = within(measured ?? box, box);
    const pathAt = (at: number): AirspaceRect => {
      const e = easeInOutCubic(at);
      return {
        left: slot.left + (box.left - slot.left) * e,
        top: slot.top + (box.top - slot.top) * e,
        right: slot.right + (box.right - slot.right) * e,
        bottom: slot.bottom + (box.bottom - slot.bottom) * e,
      };
    };
    const path = pathAt(progress);
    const key = `${rectKey(box)}|${rectKey(slot)}`;
    if (key !== geometryKey) {
      // Retarget from the clip on screen, as far as it fits the new box,
      // against the new path at the progress that clip was drawn at.
      if (lastClip !== null) {
        const shown = within(lastClip, box);
        const base = pathAt(lastProgress);
        correction = {
          left: shown.left - base.left,
          top: shown.top - base.top,
          right: shown.right - base.right,
          bottom: shown.bottom - base.bottom,
        };
        correctionBase = remaining(lastProgress);
      }
      geometryKey = key;
    }
    const w = correctionWeight();
    const clip = within({
      left: path.left + correction.left * w,
      top: path.top + correction.top * w,
      right: path.right + correction.right * w,
      bottom: path.bottom + correction.bottom * w,
    }, box);
    lastClip = clip;
    lastProgress = progress;
    clipPath = `inset(${clip.top - box.top}px ${box.right - clip.right}px ${box.bottom - clip.bottom}px ${clip.left - box.left}px)`;
    setPromotedLayer(section, clip);
  }

  function release(): void {
    cancelFrame();
    direction = 0;
    progress = 0;
    startSlot = null;
    resetPath();
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
    startSlot = plainRect(section.getBoundingClientRect());
    resetPath();
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
      if (direction === 0) resetPath();
      turn(-1);
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
        turn(1);
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
