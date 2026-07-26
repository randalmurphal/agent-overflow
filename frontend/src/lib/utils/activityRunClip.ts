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
