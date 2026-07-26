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
): ActivityRunMountWindow | null {
  const row = activityRunRowIndexOfItem(run, itemId);
  if (row < 0) return null;
  const rows = Math.max(1, run.mountedRows);
  return windowEndingAt(run, Math.max(0, row - Math.floor(rows / 2)), rows);
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

/** The registry surface `revealActivityRunItem` drives. */
export interface ActivityRunReveal {
  setCollapsed(runId: string, collapsed: boolean): void;
  setMountWindow(runId: string, window: ActivityRunMountWindow): void;
  requestFocus(runId: string, itemId: string): void;
}

/**
 * Point a run at one of its items: expand it from a chip, relocate the mount
 * window around the target, and leave the focus request the run's row
 * consumes once it is mounted. Returns false when the item is not in the run.
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
  registry.setMountWindow(run.runId, window);
  registry.requestFocus(run.runId, itemId);
  return true;
}
