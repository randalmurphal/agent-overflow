// Bringing a pane into view in the pane strip: the deferred reveal request,
// the glide that carries the strip there, and the instant snap.

import { createAnimationFrameBatcher } from '../../utils/animationFrameBatcher';

export type PaneRevealAlignment = 'start' | 'end';

const paneStripRevealFrames = createAnimationFrameBatcher('pane-strip-reveal', 'before-dom-update');

export interface PaneStripRevealOptions {
  /** The strip's scroller. */
  getHostEl: () => HTMLElement | undefined;
  /** Where a pane sits in the strip: its section, or its slot while promoted. */
  slotOf: (paneId: string) => HTMLElement | null;
  /**
   * Whether the strip may not move for this reveal now (a pane is promoted).
   * The owner decides what happens to the reveal instead.
   */
  held: (paneId: string) => boolean;
  /**
   * Whether a glide yields to a scroll it did not make. It does on a strip
   * a person scrolls. On one that only reveals move, it carries on to its
   * pane from where that scroll left the strip.
   */
  yields: () => boolean;
}

export interface PaneStripReveal {
  /** Reveals a pane on the next frame; `instant` skips the glide. */
  request(paneId: string, opts?: { instant?: boolean }): void;
  /** Aligns a pane now, dropping any glide. */
  snap(paneEl: HTMLElement): void;
  /** Stops the glide where it is. */
  cancelGlide(): void;
  /** Stops the glide and drops a reveal not yet started. */
  cancel(): void;
  /** Re-resolves a running glide after the strip's layout changed. */
  resolveAfterLayout(): void;
  /** A reveal is requested or on its way. */
  readonly active: boolean;
  /** Whether the strip is where a reveal last put it. */
  placed(el: HTMLElement): boolean;
}

/**
 * Single home for the strip's scroll bounds: every scrollLeft the strip
 * targets (reconcile clamp, glide destination, instant snap) goes through
 * here so a bounds change cannot drift between them.
 */
export function clampStripScrollLeft(el: HTMLElement, left: number): number {
  const maxScrollLeft = Math.max(0, el.scrollWidth - el.clientWidth);
  return Math.max(0, Math.min(maxScrollLeft, left));
}

// The glide is self-owned rather than native
// scrollIntoView({ behavior: 'smooth' }): browsers restart an interrupted
// smooth scrollIntoView from stale animation bookkeeping, so chained reveals
// (alt+h/l held across several panes) visibly rewound the strip before
// re-animating. An exponential approach retargets from the CURRENT position
// on every request, so chained reveals read as one continuous motion.
const GLIDE_TIME_CONSTANT_MS = 90; // ~95% of the distance covered in ~270ms
// Stalled-frame clamp: resuming mid-glide advances at glide pace instead of
// paying the whole gap in one alpha≈1 write (an unintentional snap).
const GLIDE_MAX_FRAME_MS = 40;
const GLIDE_ARRIVED_PX = 0.75;

export function createPaneStripReveal(options: PaneStripRevealOptions): PaneStripReveal {
  // A request is always deferred one frame: a reveal is asked for in the same
  // tick as the layout mutation it follows (pane open, reorder, destroy), so
  // measuring now would read pre-flush geometry. Same-frame requests coalesce
  // to the latest target. Instant wins the frame regardless of request order:
  // a reveal landing in the strip's first-paint frame still starts from a
  // position the user never saw, so gliding it would animate from nowhere.
  let pendingPaneId: string | null = null;
  let pendingInstant = false;
  let pendingRequestFrame = 0;

  let targetLeft: number | null = null;
  let targetPaneId: string | null = null;
  let targetAlignment: PaneRevealAlignment | null = null;
  // Mirrored as a float because scrollLeft reads back rounded, which would
  // stall the tail of the ease.
  let currentLeft = 0;
  let lastTs: number | null = null;
  let frame = 0;
  // The strip's scrollLeft as a reveal last left it (a glide's frame, its
  // landing, a snap), read back after each write (the browser rounds). A
  // different value means something else scrolled the strip: a
  // scroll-into-view, a scrollbar drag, a keyboard scroll.
  let leftAsWritten = Number.NaN;

  function placed(el: HTMLElement): boolean {
    return Math.abs(el.scrollLeft - leftAsWritten) <= 0.5;
  }

  function place(el: HTMLElement, left: number): void {
    el.scrollLeft = left;
    leftAsWritten = el.scrollLeft;
  }

  function cancelGlide(): void {
    if (frame) paneStripRevealFrames.cancel(frame);
    frame = 0;
    targetLeft = null;
    targetPaneId = null;
    targetAlignment = null;
    lastTs = null;
  }

  function step(ts: number): void {
    const el = options.getHostEl();
    if (!el || targetLeft === null) {
      frame = 0;
      return;
    }
    if (!placed(el)) {
      // A glide that yields leaves the strip to the scroll rather than
      // writing its own position back over the move.
      if (options.yields()) {
        cancelGlide();
        return;
      }
      currentLeft = el.scrollLeft;
    }
    const dt = lastTs === null ? 16 : Math.min(GLIDE_MAX_FRAME_MS, Math.max(0, ts - lastTs));
    lastTs = ts;
    const alpha = 1 - Math.exp(-dt / GLIDE_TIME_CONSTANT_MS);
    currentLeft += (targetLeft - currentLeft) * alpha;
    if (Math.abs(targetLeft - currentLeft) < GLIDE_ARRIVED_PX) {
      place(el, targetLeft);
      cancelGlide();
      return;
    }
    place(el, currentLeft);
    frame = paneStripRevealFrames.request(step);
  }

  function glideTo(el: HTMLElement, left: number, paneId: string, alignment: PaneRevealAlignment): void {
    targetLeft = clampStripScrollLeft(el, left);
    targetPaneId = paneId;
    targetAlignment = alignment;
    if (frame) return; // retargeted mid-flight: the running loop picks it up
    currentLeft = el.scrollLeft;
    leftAsWritten = el.scrollLeft;
    lastTs = null;
    frame = paneStripRevealFrames.request(step);
  }

  function alignedLeft(el: HTMLElement, paneEl: HTMLElement, alignment: PaneRevealAlignment): number {
    return alignment === 'start' ? paneEl.offsetLeft : paneEl.offsetLeft + paneEl.offsetWidth - el.clientWidth;
  }

  // Where the strip must scroll to show the whole pane, or null when a strip
  // at `viewLeft` already does.
  function revealTarget(
    el: HTMLElement,
    paneEl: HTMLElement,
    viewLeft: number,
  ): { left: number; alignment: PaneRevealAlignment } | null {
    const paneLeft = paneEl.offsetLeft;
    const paneRight = paneLeft + paneEl.offsetWidth;
    const clientWidth = el.clientWidth;
    if (paneLeft >= viewLeft && paneRight <= viewLeft + clientWidth) return null;
    const alignment: PaneRevealAlignment = paneLeft < viewLeft ? 'start' : 'end';
    const left = alignment === 'start' ? paneLeft : paneRight - clientWidth;
    return { left, alignment };
  }

  function snap(paneEl: HTMLElement): void {
    const el = options.getHostEl();
    if (!el) return;
    cancelGlide();
    const target = revealTarget(el, paneEl, el.scrollLeft);
    if (target !== null) place(el, clampStripScrollLeft(el, target.left));
  }

  function glideIntoView(paneEl: HTMLElement, paneId: string): void {
    const el = options.getHostEl();
    if (!el) return;
    // Judge visibility against where the strip is HEADED, not just where it
    // is: revealing a pane that is visible now but not at the destination
    // must retarget the glide, not let it carry the pane off screen.
    const target = revealTarget(el, paneEl, targetLeft ?? el.scrollLeft);
    if (target !== null) glideTo(el, target.left, paneId, target.alignment);
  }

  function request(paneId: string, opts?: { instant?: boolean }): void {
    if (options.held(paneId)) return;
    const alreadyScheduled = pendingPaneId !== null;
    pendingPaneId = paneId;
    pendingInstant = pendingInstant || (opts?.instant ?? false);
    if (alreadyScheduled) return;
    pendingRequestFrame = paneStripRevealFrames.request(() => {
      pendingRequestFrame = 0;
      const target = pendingPaneId;
      const instant = pendingInstant;
      pendingPaneId = null;
      pendingInstant = false;
      if (!target || options.held(target)) return;
      const paneEl = options.slotOf(target);
      if (!paneEl) return;
      if (instant) snap(paneEl);
      else glideIntoView(paneEl, target);
    });
  }

  // A structural change can move the pane a glide is heading for even when
  // the old numeric destination stays legal. Re-resolve from the pane's
  // current offset; a pane that is gone ends the glide rather than writing
  // stale geometry back.
  function resolveAfterLayout(): void {
    const el = options.getHostEl();
    if (!el || targetLeft === null) return;
    const paneEl = targetPaneId ? options.slotOf(targetPaneId) : null;
    if (!paneEl || targetAlignment === null) {
      cancelGlide();
      return;
    }
    targetLeft = clampStripScrollLeft(el, alignedLeft(el, paneEl, targetAlignment));
    currentLeft = el.scrollLeft;
    leftAsWritten = el.scrollLeft;
    if (Math.abs(targetLeft - currentLeft) < GLIDE_ARRIVED_PX) cancelGlide();
  }

  return {
    request,
    snap,
    cancelGlide,
    cancel() {
      cancelGlide();
      if (pendingRequestFrame) {
        paneStripRevealFrames.cancel(pendingRequestFrame);
        pendingRequestFrame = 0;
      }
      pendingPaneId = null;
      pendingInstant = false;
    },
    resolveAfterLayout,
    get active() {
      return targetLeft !== null || pendingPaneId !== null;
    },
    placed,
  };
}
