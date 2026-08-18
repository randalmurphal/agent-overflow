// The geometric half of activity-run auto-collapse: is this settled run far
// enough from the reader that collapsing it instantly cannot be seen?
//
// A run that opened as the timeline's newest activity keeps rendering open
// after prose displaces it (`openedLive` in
// stores/threadActivityRuns.svelte.ts) — snapping
// shut on the settle frame would remove a viewport of content in front of
// whoever watched it stream, and animating the removal was tried and
// rejected: no physics makes a full-viewport height change pleasant while
// the reader is looking at it. So the collapse is not softened, it is
// SCHEDULED — deferred until it is provably invisible, then applied
// instantly. This predicate is the "provably invisible" part; the reader
// half (nothing expanded inside, nothing pinned)
// lives with the gate in components/chat/timelineActivityRunAutoCollapse.ts,
// because it reads registries a pure function should not.
//
// Both conditions are content-space, from the engine's cached geometry —
// never DOM reads, which would force layout on the prune cadence.

export interface ActivityRunGateGeometry {
  /** Content-space top of the run's timeline row. */
  top: number;
  /** Content-space bottom: the next row's offset, or `totalSize` for the
   *  last revealed row. */
  bottom: number;
  /** Current scroll offset of the pane. */
  scrollTop: number;
  /** Pane viewport height. Callers bail before this can be 0 — an
   *  unmeasured scroller would call every run out of sight. */
  viewport: number;
  /** Total content height the engine reports. */
  totalSize: number;
}

/**
 * How far past the viewport's edge a run must sit before it counts as out of
 * sight. A run a hair off-screen is one nudge from being back in it, and the
 * reader would find it collapsed with no idea when that happened.
 */
export const AUTO_COLLAPSE_VIEWPORT_MARGIN_PX = 48;

/**
 * How far the run's bottom must be from the CONTENT's end, in viewports.
 * Distance from the tail rather than from the viewport, deliberately: a
 * reader who scrolls up to check something and returns to the bottom must
 * find the latest runs exactly as they left them — "the reader has moved on"
 * is a claim about where the conversation's newest activity is, not about
 * where their viewport briefly went.
 */
export const AUTO_COLLAPSE_TAIL_DISTANCE_VIEWPORTS = 1;

/**
 * True when collapsing the run now cannot move anything the reader sees:
 * the run is fully outside the viewport by a margin, AND well behind the
 * conversation's tail.
 *
 * Fully-above is the load-bearing side for a bottom-pinned reader — the
 * shrink happens above them and the engine's anchoring cancels it exactly.
 * Fully-below (a reader scrolled far up, run between them and the tail)
 * moves nothing above the viewport by construction. Partially visible is
 * never eligible, whatever the tail distance says.
 */
export function activityRunOutOfSight(g: ActivityRunGateGeometry): boolean {
  const fullyAbove = g.bottom < g.scrollTop - AUTO_COLLAPSE_VIEWPORT_MARGIN_PX;
  const fullyBelow =
    g.top > g.scrollTop + g.viewport + AUTO_COLLAPSE_VIEWPORT_MARGIN_PX;
  if (!fullyAbove && !fullyBelow) return false;
  return (
    g.totalSize - g.bottom
    > g.viewport * AUTO_COLLAPSE_TAIL_DISTANCE_VIEWPORTS
  );
}
