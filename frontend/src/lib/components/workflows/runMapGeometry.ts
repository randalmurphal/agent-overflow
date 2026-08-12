// The run map's scroll arithmetic (RUN-MAP.md §9), as pure functions over
// elements and rects.
//
// It is split out of `runMapFollow.svelte.ts` because the two answer different
// kinds of question. Everything here is "where is this box relative to that
// one" — no state, no listeners, no writes, and no opinion about whether a
// write should happen. The controller keeps the state machine: engagement, the
// glide, escape, the chip, the resize cadence, and the one write chokepoint.
//
// Splitting them is what makes the anchor DESCENT directly testable. It is the
// single rule on this surface with a non-obvious answer (see `pickAnchor`), and
// through the controller it could only ever be observed as a compensation
// number, which is two rules at once.
//
// Origin convention, shared by everything below: a scroller's origin is its
// border-box top, and its viewport height is `clientHeight`. They differ by the
// scroller's top border, which is 0 on the overlay body.

/** Band the follow target must stay inside before follow will move it. */
export const BAND_TOP_FRACTION = 0.15;
export const BAND_BOTTOM_FRACTION = 0.7;
/**
 * Where a glide parks the target: the resting line inside the band.
 *
 * EXPORTED because the controller's test has to drive the same number — a
 * restated literal there is a second place it lives, and a tuning change would
 * leave the test asserting the old contract while passing. Same precedent as
 * `INVALIDATE_DEBOUNCE_MS`.
 */
export const BAND_REST_FRACTION = 0.3;

/**
 * Runaway guard on the anchor descent, not a semantic limit — the search runs
 * to the deepest element that straddles the viewport top, and this only stops
 * it if a DOM ever nests deeper than any real one does.
 */
export const ANCHOR_MAX_DESCENT = 32;

export function maxScrollTop(el: HTMLElement): number {
  return Math.max(0, el.scrollHeight - el.clientHeight);
}

export function canScroll(el: HTMLElement): boolean {
  return el.scrollHeight > el.clientHeight;
}

/** Target's top edge, in pixels below the scroller's viewport top. */
export function targetOffset(el: HTMLElement, target: HTMLElement): number {
  return target.getBoundingClientRect().top - el.getBoundingClientRect().top;
}

export function inBand(el: HTMLElement, target: HTMLElement): boolean {
  const height = el.clientHeight;
  const offset = targetOffset(el, target);
  return offset >= height * BAND_TOP_FRACTION && offset <= height * BAND_BOTTOM_FRACTION;
}

export function isOffscreen(el: HTMLElement, target: HTMLElement): boolean {
  const rect = el.getBoundingClientRect();
  const targetRect = target.getBoundingClientRect();
  return targetRect.bottom <= rect.top || targetRect.top >= rect.top + el.clientHeight;
}

/**
 * §9.8: does THIS fold animate? On-screen folds animate; an off-screen one
 * applies instantly.
 *
 * The rule is a scroll rule, not a taste one. A height transition spends 200ms
 * changing the document, while the anchor compensation that cancels it is
 * measured once, at the flush — so an animated fold ABOVE a reader who is not
 * following drifts their viewport for every frame after the first. Instant
 * makes the whole delta land inside the hold, which is the one moment
 * compensation can see it.
 *
 * Answered against the SCROLLER's viewport rather than the window's: the map
 * lives in an overlay card, and a region below the card's fold is off-screen
 * for this reader whatever the window thinks.
 *
 * A missing scroller or region answers "animate": that is the harmless side —
 * a visible fold that should have been instant is a cosmetic miss, while an
 * off-screen fold treated as visible is the viewport drift above.
 */
export function foldAnimates(
  scroller: HTMLElement | null,
  region: HTMLElement | null,
  reducedMotion: boolean,
): boolean {
  if (reducedMotion) return false;
  if (!scroller || !region) return true;
  return !isOffscreen(scroller, region);
}

/** scrollTop that parks the target on the resting line, clamped to range. */
export function restingScrollTop(el: HTMLElement, target: HTMLElement): number {
  const raw = el.scrollTop + targetOffset(el, target) - el.clientHeight * BAND_REST_FRACTION;
  return Math.max(0, Math.min(maxScrollTop(el), raw));
}

/**
 * §9.2, last clause: a live selection inside the map holds follow writes.
 * Placement, jumps and compensation are exempt — the first two are the reader's
 * own action and the third is net-zero by construction, so neither can pull
 * text out from under a selection.
 */
export function hasSelectionInside(el: HTMLElement): boolean {
  const selection = typeof window === 'undefined' ? null : (window.getSelection?.() ?? null);
  if (!selection || selection.isCollapsed || selection.rangeCount === 0) return false;
  return el.contains(selection.getRangeAt(0).commonAncestorContainer);
}

/** First child of `scope` whose box reaches the viewport top line or below. */
export function firstReaching(scope: Element, top: number): Element | null {
  const children = scope.children;
  for (let i = 0; i < children.length; i++) {
    const child = children[i];
    if (child.getBoundingClientRect().bottom > top) return child;
  }
  return null;
}

/**
 * The element whose top edge must not move: the DEEPEST one straddling (or
 * first below) the viewport top, found by descending from the scroller.
 *
 * The descent is the whole mechanism, not a refinement, and it has to run all
 * the way down. Every ancestor of the anchor CONTAINS whatever grew above it,
 * so its own top edge does not move — measuring one is measuring zero. The
 * overlay body has exactly one child (the run detail's wrapper), which spans
 * the document; the map's own root spans the map; the wave's body spans the
 * wave. Stop at any of them and a fold, a wave that grew and a whole refetched
 * view all compensate nothing at all, silently, which is how this behaved
 * before the descent existed. Only the row-level element the reader is actually
 * looking at moves by the delta that has to be cancelled.
 *
 * A deep anchor is likelier to be REPLACED by a wholesale re-render than a
 * shallow one, and a replaced anchor cannot be restored — the hold is then
 * dropped (`captureAnchor`). That degrades to no compensation, never to a wrong
 * scroll write, which is the right way round: the shallow alternative is not
 * compensation that survives, it is compensation that never happens.
 */
export function pickAnchor(el: HTMLElement): Element | null {
  const top = el.getBoundingClientRect().top;
  let anchor = firstReaching(el, top);
  // Everything is above the line (a degenerate over-scroll): hold the first
  // child rather than nothing, so the document itself stays put.
  if (anchor === null) return el.children[0] ?? null;
  for (let depth = 1; depth < ANCHOR_MAX_DESCENT; depth++) {
    const deeper = firstReaching(anchor, top);
    if (deeper === null) break;
    anchor = deeper;
  }
  return anchor;
}
