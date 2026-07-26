// The mount window inside an activity run: which of its rows sit in the DOM,
// and how a jump relocates that window around a target.
//
// A run is ONE virtualizer row, so the DOM bound the virtualizer provides at
// top level has to be re-established inside it — without a window, a 500-row
// run would mount 500 rows the moment its single row entered the buffer.
//
// The window is (size, start) where the start is an ITEM ID, not an index.
// Both of a run's edges move: lazy older-paging extends it backward and the
// live-window prune trims its head, so an index would silently slide the
// window across the run's content. A null start means "the run's tail",
// which is the default and where the window RETURNS as soon as nothing is
// hidden below it — that is what makes a jumped-into run start following new
// activity again once the reader has caught up with it.

import {
  nodeContainsItem,
  timelineNodeItemId,
  type ActivityRunNode,
} from './subagentGrouping';

/**
 * How many of a run's rows are mounted by default. Sized to overfill the
 * clip's cap so the tail always has content below the fold. User-tunable
 * through the `activityRunWindowRows` setting; the bounds are enforced on
 * both the Go and frontend sides.
 */
export const ACTIVITY_RUN_WINDOW_ROWS_DEFAULT = 30;
export const ACTIVITY_RUN_WINDOW_ROWS_MIN = 10;
export const ACTIVITY_RUN_WINDOW_ROWS_MAX = 200;

/** Rows mounted per click on a run's "N earlier" / "N later" boundary. */
export const ACTIVITY_RUN_CHUNK_ROWS = 25;

/**
 * Which rows of a run are mounted. `rows` is the size; `startItemId` names
 * the item whose row is mounted first, or null for the run's tail.
 */
export interface ActivityRunMountWindow {
  rows: number;
  startItemId: string | null;
}

/**
 * A mount window plus the row it resolves to. The resolved start is what tells
 * a jump whether the window actually moved, which the stored `(rows,
 * startItemId)` pair cannot: tail mode says "the last rows", not which ones.
 */
export interface ActivityRunFocusWindow extends ActivityRunMountWindow {
  from: number;
}

/** A run row's pending "bring this item into view". */
export interface ActivityRunFocusRequest {
  itemId: string;
  /**
   * The mount window moved for this request, so the clip's current offset
   * refers to rows that are no longer there. The row consuming the request
   * places the target instead of leaving it wherever the stale offset put it.
   */
  relocated: boolean;
}

/**
 * Index of the run row that carries `itemId`, or -1. Recursive through group
 * cards, so a hit on a subagent's own tool call resolves to the card's row.
 */
export function activityRunRowIndexOfItem(run: ActivityRunNode, itemId: string): number {
  return run.children.findIndex((child) => nodeContainsItem(child, itemId));
}

/**
 * The window that brings `itemId` into view, or null when the item is not in
 * this run.
 *
 * Same SIZE as the current window: a jump RELOCATES the window rather than
 * growing it, so landing on the first row of a 500-row run mounts exactly as
 * many rows as landing on its last. Anchored half a window above the target
 * so the reader sees what led up to it — the inner equivalent of the outer
 * timeline's `align: 'center'`.
 */
export function activityRunFocusWindow(
  run: ActivityRunNode,
  itemId: string,
): ActivityRunFocusWindow | null {
  const row = activityRunRowIndexOfItem(run, itemId);
  if (row < 0) return null;
  const rows = Math.max(1, run.mountedRows);
  const from = clampWindowStart(run, row - Math.floor(rows / 2), rows);
  return { ...windowEndingAt(run, from, rows), from };
}

/** Grow the window upward by one chunk, keeping its bottom edge. */
export function activityRunWindowGrownOlder(run: ActivityRunNode): ActivityRunMountWindow {
  const end = run.mountedFrom + run.mountedRows;
  const from = Math.max(0, run.mountedFrom - ACTIVITY_RUN_CHUNK_ROWS);
  return windowEndingAt(run, from, end - from);
}

/** Grow the window downward by one chunk, keeping its top edge. */
export function activityRunWindowGrownNewer(run: ActivityRunNode): ActivityRunMountWindow {
  const end = Math.min(
    run.children.length,
    run.mountedFrom + run.mountedRows + ACTIVITY_RUN_CHUNK_ROWS,
  );
  return windowEndingAt(run, run.mountedFrom, end - run.mountedFrom);
}

function windowEndingAt(
  run: ActivityRunNode,
  from: number,
  rows: number,
): ActivityRunMountWindow {
  // Tail mode whenever the window reaches the run's last row: an anchored
  // window freezes where the reader stopped, which is right while rows are
  // hidden below and wrong once none are.
  if (from + rows >= run.children.length) return { rows, startItemId: null };
  return { rows, startItemId: timelineNodeItemId(run.children[from]) };
}

/**
 * Where a window of `rows` really starts when asked to start at `from`: never
 * before the run's first row, and never past the start that puts its last row
 * on the run's last row. One rule, so a caller reporting the resolved start
 * cannot disagree with what the registry mounts.
 */
function clampWindowStart(run: ActivityRunNode, from: number, rows: number): number {
  return Math.min(Math.max(0, from), Math.max(0, run.children.length - rows));
}

/** The registry surface `revealActivityRunItem` drives. */
export interface ActivityRunReveal {
  setCollapsed(runId: string, collapsed: boolean): void;
  setWindowAnchor(runId: string, anchorItemId: string | null): void;
  /** False when the registry holds no such run; see `ThreadActivityRuns`. */
  requestFocus(runId: string, request: ActivityRunFocusRequest): boolean;
}

/**
 * Point a run at one of its items: expand it from a chip, relocate the mount
 * window around the target, and leave the focus request the run's row
 * consumes once it is mounted.
 *
 * Returns false when the item is not in the run, and when the registry no
 * longer holds the run at all — a node resolved before a sweep or a thread
 * switch names an id whose entry is gone, and every mutator here is a no-op
 * for it. The answer comes from the registry's own report rather than a
 * pre-check, so it cannot drift from what was actually stored.
 *
 * One function rather than three calls at each jump site because a partial
 * application is a silent bug — a relocated window on a collapsed run shows
 * nothing, and a focus request the window doesn't cover scrolls nowhere.
 */
export function revealActivityRunItem(
  registry: ActivityRunReveal,
  run: ActivityRunNode,
  itemId: string,
): boolean {
  const window = activityRunFocusWindow(run, itemId);
  if (!window) return false;
  registry.setCollapsed(run.runId, false);
  // The anchor, not a whole window: a jump moves the window and never resizes
  // it, and asking for the size it already has would record that size as an
  // explicit override — pinning a short run at its current length so it stops
  // widening as it grows.
  registry.setWindowAnchor(run.runId, window.startItemId);
  return registry.requestFocus(run.runId, {
    itemId,
    relocated: window.from !== run.mountedFrom,
  });
}
