// The automatic close of a working run's clip.
//
// A run nobody has answered for shows its work while it has any: the thread's
// default (and the `activityRunDefault` setting behind it) describes how a run
// sits once it has SETTLED, so a live one renders open and the reader watches
// the work land instead of watching a number tick. When it stops being live the
// default takes over and that clip has to go away, and the one thing it must not
// do is teleport — a run that vanished between two frames is exactly the jump
// every other transaction in this area exists to avoid.
//
// Which is also why this is the only close that animates. A collapse the reader
// clicked is instant, including on a live run: they asked, and the answer is the
// clip going away.
//
// So it folds: the clip's box animates to zero height while the clip itself is
// pinned to the closing edge, which makes the run close ONTO its newest row
// with the older ones sliding out of the top. The timeline's own machinery
// carries the rest — the row's ResizeObserver measures the shrinking box every
// frame, the virtualizer's engine restates the offsets below it, and the scroll
// controller decides what that means for the reading position. Nothing here
// writes `scrollTop`.
//
// Pure by design: the numbers are the tunable part, and they are the part
// worth testing without a DOM.

/**
 * Shortest fold. Below this the motion reads as a flicker, not a close.
 *
 * This is the knob that sets the pace, not the ceiling below: it is most of
 * every real fold, because a capped clip is 8 rows
 * (`ACTIVITY_RUN_CAP_ROWS` = 288px) and the height below buys ~130ms of that.
 * The ceiling only binds for a run whose expanded bodies lifted its cap well
 * past the default.
 */
const FOLD_MIN_MS = 320;
/** Longest fold. Past this a finished run is still in the way. */
const FOLD_MAX_MS = 600;
/**
 * How much of the duration the height buys. Deliberately sub-linear: a 400px
 * run and a 120px one should feel like the same gesture at different sizes,
 * not like one taking three times as long to admit it is done.
 */
const FOLD_MS_PER_PX = 0.45;

/**
 * Starts on the first frame, fastest through the middle, and settles rather
 * than stops.
 *
 * Three things the curve has to avoid, and each one is a control point.
 *
 * It must not hold still first. A textbook ease-in (y1 = 0) leaves the box
 * motionless for roughly the opening third and crams the close into what is
 * left, so the fold reads far sharper than its duration says. y1 off zero puts
 * motion in the first frame.
 *
 * It must not LAND at full speed. With the second point at (1, 1) the box is
 * moving fastest at the instant it reaches zero and the motion stops dead — the
 * close reads as violent no matter how long it took, which is what a slower
 * duration alone could not fix. x2 short of 1 gives the tangent somewhere to
 * point, so the height eases into its landing.
 *
 * And it must not crawl there. The further x2 drops the more of the duration is
 * spent covering the last few pixels, which reads as the run hesitating — done
 * well before it is finished. Keeping x2 late confines the deceleration to the
 * tail.
 */
export const ACTIVITY_RUN_FOLD_EASING = 'cubic-bezier(0.3, 0.2, 0.8, 1)';

/** How long a clip of `px` height takes to fold shut. */
export function activityRunFoldDurationMs(px: number): number {
  if (!(px > 0)) return FOLD_MIN_MS;
  const scaled = FOLD_MIN_MS + px * FOLD_MS_PER_PX;
  return Math.round(Math.min(FOLD_MAX_MS, Math.max(FOLD_MIN_MS, scaled)));
}

/**
 * Grace past the animation's own duration before the fold is finished by hand.
 *
 * A backgrounded tab stops advancing the document timeline, so an animation
 * started just before a switch away would otherwise hold the clip open — and
 * the run behind it — until the reader came back. Timers keep running (coarsely)
 * where animations do not, which is exactly why the deadline is one.
 */
export function activityRunFoldDeadlineMs(durationMs: number): number {
  return durationMs + 400;
}

/**
 * The reader asked for no animation, so the fold is a state change like any
 * other manual collapse.
 *
 * A capability check rather than a guard against failure: `matchMedia` is
 * absent in some test environments, and its absence means "no stated
 * preference", not an error to report.
 */
export function prefersReducedMotion(): boolean {
  if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') {
    return false;
  }
  return window.matchMedia('(prefers-reduced-motion: reduce)').matches;
}
