// Geometry for an activity run's height-capped clip.
//
// The cap bounds how much vertical space a stretch of ACTIVITY takes. An
// expanded payload is not activity — it is content the user explicitly
// asked to read — so the cap grows by exactly what expansion added, and
// reading a diff inside a run never means scroll-within-scroll.
//
// Expanded bodies are found through the disclosure contract rather than a
// marker attribute rows have to remember: an expandable row header is a
// `TranscriptDisclosureHeader`, which emits `aria-expanded` and points at
// its body with `aria-controls`. A body that skipped the query would be an
// accessibility defect first, so the coupling pushes in the right
// direction — a new expandable body cannot silently opt out of the cap
// while staying correct for a screen reader.

/** Base cap, before any expansion. Tunable; feel-tuned against real runs. */
export const ACTIVITY_RUN_CAP_CSS = 'min(50vh, 32rem)';

export function activityRunClipMaxHeight(expandedPx: number): string {
  if (expandedPx <= 0) return ACTIVITY_RUN_CAP_CSS;
  return `calc(${ACTIVITY_RUN_CAP_CSS} + ${Math.round(expandedPx)}px)`;
}

/**
 * Bodies currently revealed by a disclosure inside `clip`.
 *
 * Ids are resolved WITHIN the clip, never through `document`: a body
 * outside this run must not lift this run's cap, and a stale
 * `aria-controls` pointing at a since-unmounted id resolves to nothing
 * instead of to whatever else claimed that id.
 */
export function activityRunExpandedBodies(clip: Element): HTMLElement[] {
  const found: HTMLElement[] = [];
  const seen = new Set<string>();
  for (const trigger of clip.querySelectorAll('[aria-expanded="true"][aria-controls]')) {
    const id = trigger.getAttribute('aria-controls');
    if (!id || seen.has(id)) continue;
    seen.add(id);
    const body = clip.querySelector(`[id="${CSS.escape(id)}"]`);
    if (body instanceof HTMLElement) found.push(body);
  }
  // A disclosure nested inside another expanded body (a tool row inside an
  // expanded subagent card) is already accounted for by its ancestor's
  // height. Counting both would lift the cap twice for one expansion.
  return found.filter((body) => !found.some((other) => other !== body && other.contains(body)));
}

export function activityRunExpandedHeight(bodies: readonly HTMLElement[]): number {
  let total = 0;
  for (const body of bodies) total += body.offsetHeight;
  return total;
}

/**
 * Report the total height of `clip`'s expanded bodies, and keep reporting it
 * as they open, close, and resize. Returns a teardown.
 *
 * Two observers, because the two things that change the number are unrelated:
 * a disclosure toggling (an `aria-expanded` mutation) changes WHICH bodies
 * count, and a body growing (a diff loading, output streaming into an open
 * card) changes what one of them contributes.
 */
export function observeActivityRunExpansion(
  clip: HTMLElement,
  onHeight: (px: number) => void,
): () => void {
  const sizes = new ResizeObserver(() => {
    onHeight(activityRunExpandedHeight(activityRunExpandedBodies(clip)));
  });
  function retarget(): void {
    const bodies = activityRunExpandedBodies(clip);
    sizes.disconnect();
    for (const body of bodies) sizes.observe(body);
    onHeight(activityRunExpandedHeight(bodies));
  }
  const disclosures = new MutationObserver(retarget);
  disclosures.observe(clip, {
    subtree: true,
    attributes: true,
    attributeFilter: ['aria-expanded'],
  });
  retarget();
  return () => {
    disclosures.disconnect();
    sizes.disconnect();
  };
}

/**
 * The mounted wrapper for run row `index`, or null when that row is outside
 * the mount window.
 *
 * Rows are addressed by index rather than by item id because only leaf rows
 * carry `data-item-id` — a jump into a subagent card inside a run has to
 * resolve to the card, which owns no id attribute of its own.
 */
export function activityRunChildElement(clip: Element, index: number): HTMLElement | null {
  const found = clip.querySelector(`[data-run-child="${index}"]`);
  return found instanceof HTMLElement ? found : null;
}

/**
 * Whether `row` is entirely inside `clip`'s viewport.
 *
 * Separate from the centering below because the two are combined by a policy
 * that depends on the jump, not on the geometry: a jump that RELOCATED the
 * mount window has to place its target (the offset it inherited pointed at
 * different rows, so wherever the target landed is an accident), while a jump
 * the window already covered must leave a visible target alone — the reader is
 * looking at those rows, and nudging them would be the jump fighting them.
 */
export function activityRunRowFullyVisible(clip: HTMLElement, row: HTMLElement): boolean {
  const top = row.getBoundingClientRect().top - clip.getBoundingClientRect().top;
  return top >= 0 && top + row.offsetHeight <= clip.clientHeight;
}

/**
 * `scrollTop` that centers `row` in `clip`. Floored at 0; the browser clamps
 * the other end, so a row near either edge lands as close to centered as the
 * run allows.
 */
export function activityRunCenteredScrollTop(clip: HTMLElement, row: HTMLElement): number {
  const top = row.getBoundingClientRect().top - clip.getBoundingClientRect().top;
  const contentTop = clip.scrollTop + top;
  return Math.max(0, contentTop - Math.max(0, (clip.clientHeight - row.offsetHeight) / 2));
}
