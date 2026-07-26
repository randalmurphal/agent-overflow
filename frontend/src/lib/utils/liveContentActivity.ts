// Is live timeline content still arriving? A rolling activity window
// over the last content advance, used for LIVENESS decisions only.
//
// This deliberately does NOT choose scroll physics. Whether a growth
// glides or teleports is decided from signals that describe the growth
// itself — warm-up (mount/restore cascade), width reflow (layout
// correction, not content), reduced motion, escape/pause, and the idle
// deadband. A stopwatch on the last stamp is not one of those signals,
// and keying physics on it is what produced the two end-of-turn jump
// classes (investigation 2026-07-25): late row enrichment (highlight
// spans, KaTeX, Mermaid, image load) and any drain growth landing in a
// reveal gap both fall outside the window with nothing to re-stamp, and
// used to sync-pin as a visible snap. See
// docs/architecture/frontend-scroll.md#live-content-animation.
//
// What the window IS for: "is more content expected imminently?"
//   - The spring sentinel. After arrival with content still flowing the
//     chase stays sentinel-alive (re-rAF without writing) so
//     `springActive` holds across inter-chunk gaps — the flag the
//     resolver's negative-delta carve-out keys on. Ending it early only
//     costs a restart; the failure is cheap and self-correcting, which
//     is exactly why a time window is an acceptable answer HERE and not
//     for physics.
//   - The live-capable observation path (`observe('live-content')` /
//     `'composer-geometry'`), where a viewport-height change during
//     active output should ride the in-flight glide rather than snap
//     through it, but an idle composer resize should stay pinned.
//
// `lastLiveContentAt` is stamped on the owning ThreadPane (see
// `stores/thread.svelte.ts`) on prose/reasoning reveals, direct text
// patches, new text-like provider rows, visible-field updates to mounted
// rows, and gated wire appends / reveal releases
// (`armLiveContentAppendSpring`). Both sides use `performance.now()` so
// the comparison shares one monotonic timebase.

// Hold window after the last live-content advance during which content
// still counts as actively arriving. Pure tuning: long enough to ride
// out inter-chunk wire gaps (tool round-trips, the end-of-turn drain)
// without the sentinel dying and restarting per chunk, short enough that
// an idle pane stops re-rAFing promptly after a turn.
export const LIVE_CONTENT_ACTIVE_HOLD_MS = 500;

/**
 * True when live content advanced within `holdMs` of `now`. A
 * never-stamped pane (`lastLiveContentAt === 0`) is inactive once
 * `now >= holdMs` — i.e. immediately in practice, since `now` is a
 * `performance.now()` reading.
 *
 * Pure: `now` is passed in so the decision is trivially testable and the
 * timing concern stays in the caller / the pane stamp.
 */
export function isLiveContentActive(
  now: number,
  lastLiveContentAt: number,
  holdMs: number,
): boolean {
  return now - lastLiveContentAt < holdMs;
}
