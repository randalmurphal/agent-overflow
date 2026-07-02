// Pure latch deciding chat's autonomous-scroll animation mode: 'spring'
// (velocity-spring chase that smoothly follows the moving bottom) vs
// 'instant' (same-paint sync-pin, no perceptible motion). See
// docs/architecture/frontend-scroll.md for the two behaviors.
//
// The latch keys on WHEN smooth live timeline content last advanced — a
// text/reasoning reveal, direct text patch, or text-like provider row —
// NOT on whether a provider turn is active. Keying on content (data
// mutation) rather than turn lifecycle is what fixes the two edge bugs
// (turn ends while the agent keeps streaming; the end-of-turn
// word-by-word drain tail that reveals for seconds after the wire turn
// closes) AND keeps idle async-typesetting reflow on settled content
// sync-pinned: shiki / KaTeX / mermaid grow row height but never advance
// content, so they never refresh `lastLiveContentAt` and the latch stays
// 'instant'. Tool rows also stay sync-pinned because their virtual
// estimates often remeasure immediately after insertion.
//
// `lastLiveContentAt` is stamped on the owning ThreadPane (see
// `stores/thread.svelte.ts`); MessageTimeline reads it per-contentRO-fire
// through this function. Both sides use `performance.now()` so the
// comparison shares one monotonic timebase.

export type SpringAnimationMode = 'spring' | 'instant';

// Hold window after the last live-content advance during which the
// controller keeps spring-chasing. Pure UX tuning: long enough to ride
// out inter-chunk wire gaps (tool round-trips, the end-of-turn drain)
// without the spring settling and re-accelerating per chunk, short
// enough that idle typesetting reflow after a turn sync-pins instead of
// animating. Historically this ALSO had to stay greater than the
// controller's sentinel lifetime (`RETAIN_ANIMATION_DURATION_MS`) so
// the property-descriptor write gate stayed closed against virtua's
// direct `$fixScrollJump` writes across gaps; that cross-file invariant
// died when those writes were routed through the controller's resolver
// (`resolveVirtuaCompensation`), whose decision order is mode-free — a
// compensation arriving after the sentinel dies resolves through the
// pass/redirect tiers, both safe.
export const SPRING_MODE_HOLD_MS = 500;

/**
 * Return 'spring' when live content advanced within `holdMs` of `now`,
 * else 'instant'. A never-stamped pane (`lastLiveContentAt === 0`)
 * resolves to 'instant' once `now >= holdMs` — i.e. immediately in
 * practice, since `now` is a `performance.now()` reading.
 *
 * Pure: `now` is passed in so the decision is trivially testable and the
 * timing concern stays in the caller / the pane stamp.
 */
export function latchedSpringMode(
  now: number,
  lastLiveContentAt: number,
  holdMs: number,
): SpringAnimationMode {
  return now - lastLiveContentAt < holdMs ? 'spring' : 'instant';
}
