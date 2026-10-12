// The open thread's loops, watches and reminders for the activity rail's
// Background tray (docs/specs/standing-agents.md, "Activity rail"). It reads
// ListThreadWakes when its thread opens, after its computer reconnects or
// loses a `thread:wakes_changed` frame, and on every frame that names its
// thread, which the backend sends after each write to a loop, watch or
// reminder: registered, fired, ran, recorded an outcome, cancelled. Nothing
// polls. Reads go through the rate-bounded refresh scheduler, so a watch
// that fires every second costs a few reads per second at most.
//
// Owned beside the background controller by the composer's rail host,
// which counts both in the tray toggle and the rail's visibility. An agent
// pane's tray lists only its agent's rows and mounts none. One controller
// per composer; it holds one list, at most the thread's 64 loops and
// watches and its 64 soonest reminders.

import type { ThreadPane } from '../../stores/thread.svelte';
import { ListThreadWakes, type ThreadWakeRow } from '../../stores/bindings';
import { onThreadWakesChanged } from '../../stores/eventsThreadWakes';
import { getTransportStatusFor, onBackendStatusChange } from '../../stores/transportStatus.svelte';
import { threadBackend } from '../../transport/entityIndex';
import { createRefreshScheduler } from '../../utils/refreshScheduler';

const REFRESH_DELAY_MS = 100;
const REFRESH_MAX_WAIT_MS = 400;

export interface WakeController {
  readonly rows: ThreadWakeRow[];
  /** Reminders past the listing's bound, counted but not listed. */
  readonly moreReminders: number;
  /** Every loop, watch and reminder, listed or not: the toggle's share. */
  readonly count: number;
  /** Interval runs and fires in flight. A long-running watch's process is
   *  its standing state, not work, so it does not pulse the toggle. */
  readonly runningCount: number;
  readonly threadId: string | null;
  /** Subscribe to events; returns a disposer. Call once from onMount. */
  mount(): () => void;
}

export function createWakeController(
  getPane: () => Pick<ThreadPane, 'thread' | 'hasDraftPlaceholder'> | undefined,
): WakeController {
  let rows: ThreadWakeRow[] = $state.raw([]);
  let moreReminders = $state(0);

  // A draft pane's placeholder thread exists on no computer.
  const threadId = $derived.by(() => {
    const pane = getPane();
    return !pane || pane.hasDraftPlaceholder ? null : pane.thread?.id ?? null;
  });

  function replace(next: ThreadWakeRow[], more: number): void {
    rows = next;
    moreReminders = more;
  }

  const refresh = createRefreshScheduler({
    name: 'activityRailWakes',
    delayMs: REFRESH_DELAY_MS,
    maxWaitMs: REFRESH_MAX_WAIT_MS,
    run: async (token) => {
      const id = threadId;
      if (!id) {
        replace([], 0);
        return;
      }
      const owner = threadBackend(id);
      if (owner !== undefined && getTransportStatusFor(owner).status !== 'connected') return;
      try {
        const wakes = await ListThreadWakes(id);
        if (!token.isCurrent() || id !== threadId) return;
        replace(wakes?.rows ?? [], wakes?.moreReminders ?? 0);
      } catch (err) {
        if (!token.isCurrent() || id !== threadId) return;
        // A failed read says nothing about the schedule. Keep the last
        // list until a read succeeds or the thread changes.
        console.error('ActivityRail: ListThreadWakes failed:', err);
      }
    },
  });

  $effect(() => {
    void threadId;
    replace([], 0);
    refresh.reset();
    refresh.request({ immediate: true });
  });

  const count = $derived(rows.length + moreReminders);
  const runningCount = $derived(rows.filter((row) => row.state === 'running' && !row.longRunning).length);

  return {
    get rows() { return rows; },
    get moreReminders() { return moreReminders; },
    get count() { return count; },
    get runningCount() { return runningCount; },
    get threadId() { return threadId; },

    mount(): () => void {
      // The tray reads its list again rather than applying the frame's row:
      // the listing's order and its bound on reminders are the backend's.
      const cancelChanged = onThreadWakesChanged((change, backend) => {
        if (!threadId) return;
        if (change === null ? threadBackend(threadId) !== backend : change.threadId !== threadId) return;
        refresh.request({ immediate: change === null });
      });
      const cancelStatus = onBackendStatusChange((backend, status) => {
        if (!threadId || threadBackend(threadId) !== backend) return;
        refresh.reset();
        if (status.status === 'connected') refresh.request({ immediate: true });
      });
      return () => {
        cancelChanged();
        cancelStatus();
        refresh.dispose();
      };
    },
  };
}
