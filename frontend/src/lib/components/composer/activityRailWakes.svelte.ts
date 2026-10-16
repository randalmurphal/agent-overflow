// The open thread's loops, watches and reminders for the activity rail's
// Background tray (docs/specs/standing-agents.md, "Activity rail"), as the
// wakes store holds them (stores/threadWakes.svelte.ts). The controller
// retains its thread there while it shows it, which reads the listing when
// the thread opens and keeps it with each `thread:wakes_changed` frame;
// nothing here reads or listens. The tray lists nothing until that first
// read lands.
//
// Owned beside the background controller by the composer's rail host,
// which counts both in the tray toggle and the rail's visibility. An agent
// pane's tray lists only its agent's rows and mounts none. The listing
// holds at most the thread's 64 loops and watches and its 64 soonest
// reminders.

import type { ThreadPane } from '../../stores/thread.svelte';
import type { ThreadWakeRow } from '../../stores/bindings';
import { getThreadWakeListing, retainThreadWakes } from '../../stores/threadWakes.svelte';

export interface WakeController {
  readonly rows: ThreadWakeRow[];
  /** Reminders past the listing's bound, counted but not listed. */
  readonly moreReminders: number;
  /** Every loop, watch and reminder, listed or not: the toggle's share. */
  readonly count: number;
  /** Interval runs and fires in flight. A long-running watch's process is
   *  its standing state, not work, so it does not pulse the toggle. */
  readonly runningCount: number;
  /** Every row running, long-running watches included: the tray's own
   *  `N running`, which must agree with the rows under it. */
  readonly runningRowCount: number;
  readonly threadId: string | null;
}

export function createWakeController(
  getPane: () => Pick<ThreadPane, 'thread' | 'hasDraftPlaceholder'> | undefined,
): WakeController {
  // A draft pane's placeholder thread exists on no computer.
  const threadId = $derived.by(() => {
    const pane = getPane();
    return !pane || pane.hasDraftPlaceholder ? null : pane.thread?.id ?? null;
  });

  $effect(() => {
    if (threadId) return retainThreadWakes(threadId);
  });

  const listing = $derived(threadId ? getThreadWakeListing(threadId) : null);
  const rows = $derived(listing ? [...listing.loops, ...listing.reminders] : []);
  const moreReminders = $derived(listing?.moreReminders ?? 0);
  const count = $derived(rows.length + moreReminders);
  const runningRowCount = $derived(rows.filter((row) => row.state === 'running').length);
  const runningCount = $derived(rows.filter((row) => row.state === 'running' && !row.longRunning).length);

  return {
    get rows() { return rows; },
    get moreReminders() { return moreReminders; },
    get count() { return count; },
    get runningCount() { return runningCount; },
    get runningRowCount() { return runningRowCount; },
    get threadId() { return threadId; },
  };
}
