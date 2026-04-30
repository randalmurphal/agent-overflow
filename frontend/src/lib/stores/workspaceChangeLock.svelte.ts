import type { ThreadPane } from './thread.svelte';
import { ListLiveBackgroundTasks } from './bindings';
import { onItemUpsert, wailsEventOn } from './events';
import type { BackgroundTaskStateEvent, BackgroundTasksChangedEvent } from '../types/events';
import type { Item } from '../types/models';
import { debounce } from '../utils/debounce';

export interface WorkspaceChangeLockState {
  readonly locked: boolean;
  readonly reason: string;
  readonly runningBackgroundCount: number;
  refresh(): Promise<void>;
}

function countBlockingBackgroundTasks(items: readonly Item[]): number {
  const completedLaunches = new Set<string>();
  for (const item of items) {
    if (item.completionOf) completedLaunches.add(item.completionOf);
  }

  let count = 0;
  for (const item of items) {
    if (
      item.kind === 'tool_call' &&
      item.status === 'running' &&
      item.isBackground === true &&
      !item.completionOf &&
      !completedLaunches.has(item.id)
    ) {
      count += 1;
    }
  }
  return count;
}

export function createWorkspaceChangeLockState(getPane: () => ThreadPane): WorkspaceChangeLockState {
  let runningBackgroundCount = $state(0);
  let backgroundCheckInFlight = $state(false);
  let checkedThreadId = $state('');
  let fetchSeq = 0;

  let threadId = $derived(getPane().thread?.id ?? '');
  const debouncedRefresh = debounce(() => { void refresh(); }, 100);

  async function refresh(): Promise<void> {
    const id = threadId;
    const seq = ++fetchSeq;
    if (!id) {
      runningBackgroundCount = 0;
      backgroundCheckInFlight = false;
      checkedThreadId = '';
      return;
    }
    backgroundCheckInFlight = true;
    try {
      const items = (await ListLiveBackgroundTasks(id)) as Item[] | null;
      if (seq !== fetchSeq || id !== threadId) return;
      runningBackgroundCount = countBlockingBackgroundTasks(items ?? []);
      checkedThreadId = id;
    } catch (err) {
      if (seq !== fetchSeq || id !== threadId) return;
      console.error('workspace change lock: ListLiveBackgroundTasks failed:', err);
      runningBackgroundCount = 0;
      checkedThreadId = id;
    } finally {
      if (seq === fetchSeq && id === threadId) {
        backgroundCheckInFlight = false;
      }
    }
  }

  $effect(() => {
    threadId;
    debouncedRefresh.cancel();
    void refresh();
  });

  $effect(() => {
    const cancelItemUpsert = onItemUpsert((item) => {
      if (item.threadId !== threadId) return;
      if (item.isBackground || item.completionOf) {
        debouncedRefresh();
      }
    });
    const cancelBackgroundTasksChanged = wailsEventOn<BackgroundTasksChangedEvent>(
      'provider:background_tasks_changed',
      (evt) => {
        if (!evt || evt.threadId !== threadId) return;
        debouncedRefresh();
      },
    );
    // Background-task state events fire on host-process exit
    // (state=exited) and on agent-observation drain (state=drained).
    // Both transitions can flip the workspace lock if a backgrounded
    // task drops out of the live set.
    const cancelBackgroundTaskState = wailsEventOn<BackgroundTaskStateEvent>(
      'provider:background_task_state',
      (evt) => {
        if (!evt || evt.threadId !== threadId) return;
        debouncedRefresh();
      },
    );
    return () => {
      debouncedRefresh.cancel();
      cancelItemUpsert();
      cancelBackgroundTasksChanged();
      cancelBackgroundTaskState();
    };
  });

  return {
    get locked() {
      return getPane().isTurnActive ||
        backgroundCheckInFlight ||
        (threadId !== '' && checkedThreadId !== threadId) ||
        runningBackgroundCount > 0;
    },
    get reason() {
      if (getPane().isTurnActive) return 'Workspace changes are unavailable while the agent is responding.';
      if (runningBackgroundCount > 0) return 'Workspace changes are unavailable while background tasks are running.';
      if (backgroundCheckInFlight || (threadId !== '' && checkedThreadId !== threadId)) {
        return 'Checking workspace availability...';
      }
      return '';
    },
    get runningBackgroundCount() {
      return runningBackgroundCount;
    },
    refresh,
  };
}
