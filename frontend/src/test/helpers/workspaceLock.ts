import type { WorkspaceChangeLockState } from '../../lib/stores/workspaceChangeLock.svelte';
import type { BusyThread, WorkspaceActivity } from '../../lib/stores/bindings';

/** The GetWorkspaceActivity payload for a directory with nothing running. */
export function idleWorkspaceActivity(): WorkspaceActivity {
  return { activeTurnThreads: 0, runningBackgroundTasks: 0, busyThreads: [] } as WorkspaceActivity;
}

/** A payload with live background tasks on one thread. The thread
 *  defaults to a SIBLING the pane under test never mounted. */
export function busyWorkspaceActivity(
  runningBackgroundTasks = 1,
  threadId = 'thread-sibling',
): WorkspaceActivity {
  const busy = { threadId, activeTurn: false, runningBackgroundTasks } as BusyThread;
  return {
    activeTurnThreads: 0,
    runningBackgroundTasks,
    busyThreads: [busy],
  } as WorkspaceActivity;
}

// Inert workspace-change lock for component tests. Override `locked` /
// `reason` (directory busy) or `threadLocked` / `threadReason` (this thread
// busy) to simulate an active turn or running background tasks.
export function makeWorkspaceLock(
  overrides: Partial<WorkspaceChangeLockState> = {},
): WorkspaceChangeLockState {
  return {
    locked: false,
    reason: '',
    threadLocked: false,
    threadReason: '',
    runningBackgroundCount: 0,
    refresh: () => {},
    ...overrides,
  };
}
