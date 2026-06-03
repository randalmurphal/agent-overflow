// Pure latch deciding chat's autonomous-scroll animation mode: 'spring'
// (velocity-spring chase that smoothly follows the moving bottom) vs
// 'instant' (same-paint sync-pin, no perceptible motion). See
// frontend/AGENTS.md § Scroll architecture for the two behaviors.
//
// The latch keys on WHEN live timeline content last advanced — a text
// reveal, a streaming delta, or a new provider row — NOT on whether a
// provider turn is active. Keying on content (data mutation) rather than
// turn lifecycle is what fixes the two edge bugs (turn ends while the
// agent keeps streaming; the end-of-turn word-by-word drain tail that
// reveals for seconds after the wire turn closes) AND keeps idle
// async-typesetting reflow on settled content sync-pinned: shiki / KaTeX
// / mermaid grow row height but never advance content, so they never
// refresh `lastLiveContentAt` and the latch stays 'instant'.
//
// `lastLiveContentAt` is stamped on the owning ThreadPane (see
// `stores/thread.svelte.ts`); MessageTimeline reads it per-contentRO-fire
// through this function. Both sides use `performance.now()` so the
// comparison shares one monotonic timebase.

export type SpringAnimationMode = 'spring' | 'instant';

// Hold window after the last live-content advance during which the
// controller keeps spring-chasing. Must stay GREATER than the
// controller's spring sentinel lifetime
// (`RETAIN_ANIMATION_DURATION_MS = 350` in `useStickToBottom.svelte.ts`):
// the sentinel keeps the external-write gate closed against virtua's
// `$fixScrollJump` only while `animationMode() === 'spring'`, so the
// latch must report 'spring' for at least as long as a sentinel can be
// alive after the last stamp. Because the sentinel's own survival
// condition IS `animationMode === 'spring'`, the relationship is
// self-enforcing as long as HOLD > RETAIN. The `HOLD > RETAIN` test in
// `useStickToBottom.svelte.test.ts` pins it against the live RETAIN
// constant (both sides imported, so a bump to either side trips it).
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
