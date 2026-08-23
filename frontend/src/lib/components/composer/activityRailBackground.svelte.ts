// Background-tasks controller for the activity rail. Owns the
// `ListLiveBackgroundTasks` polling, three event subscriptions
// (`provider:item_event`-derived `onItemUpsert`,
// `provider:background_tasks_changed`, `provider:background_task_state`),
// and a debounced refresh. Exposes reactive `tasks` / `runningCount` /
// `hasPendingCompletion` for the rail's toggle pill and
// expanded body.
//
// Owned by `Composer.svelte`, not the rail: the composer's `railVisible`
// predicate reads `count`, and the rail + height-reservation spacer must
// render as complements of that one predicate — a controller living
// inside the rail would be torn down by the very unmount its count
// triggers. Lifecycle is driven by `mount(...)` (call from the host's
// `onMount`) and the returned `dispose` function (call from
// `onDestroy`). No global state — one controller per Composer mount.

import type { ThreadPane } from '../../stores/thread.svelte';
import { ListLiveBackgroundTasks } from '../../stores/bindings';
import { onItemUpsert } from '../../stores/eventsItemStream';
import { wailsEventOn } from '../../stores/wailsEvents';
import type {
  BackgroundTaskStateEvent,
  BackgroundTasksChangedEvent,
} from '../../types/events';
import type { Item } from '../../types/models';
import { asProviderID, type ProviderID } from '../../types/providers';
import { deriveTrayTasks, type TrayTask } from '../../utils/backgroundTray';
import { debounce } from '../../utils/debounce';

// Brief retention so a completion has time to flicker into view as the
// terminal state but doesn't linger after the user has read it. Just
// long enough to register; not long enough to feel sticky.
const COMPLETION_RETENTION_MS = 200;
const REFRESH_DEBOUNCE_MS = 100;

export interface BackgroundController {
  readonly tasks: TrayTask[];
  readonly count: number;
  readonly runningCount: number;
  readonly hasPendingCompletion: boolean;
  readonly threadId: string | null;
  readonly provider: ProviderID | null;
  /** Subscribe to events; returns a disposer. Call once from onMount. */
  mount(): () => void;
}

export function createBackgroundController(
  getPane: () => ThreadPane,
  getNow: () => number,
): BackgroundController {
  let backgroundItems: Item[] = $state([]);

  const threadId = $derived(getPane().thread?.id ?? null);
  const provider = $derived(asProviderID(getPane().thread?.provider));

  let fetchSeq = 0;
  async function refreshItems(): Promise<void> {
    const id = threadId;
    const seq = ++fetchSeq;
    if (!id) {
      backgroundItems = [];
      return;
    }
    try {
      const items = (await ListLiveBackgroundTasks(id)) as Item[] | null;
      if (seq !== fetchSeq) return;
      if (id !== threadId) return;
      backgroundItems = (items ?? []).filter((item) => item.threadId === id);
    } catch (err) {
      if (seq !== fetchSeq) return;
      if (id !== threadId) return;
      console.error('ActivityRail: ListLiveBackgroundTasks failed:', err);
      backgroundItems = [];
    }
  }

  const debouncedRefresh = debounce(
    () => { void refreshItems(); },
    REFRESH_DEBOUNCE_MS,
  );

  // Refetch when the thread switches.
  $effect(() => {
    threadId;
    backgroundItems = [];
    void refreshItems();
  });

  const tasks = $derived<TrayTask[]>(
    deriveTrayTasks(backgroundItems, getNow(), COMPLETION_RETENTION_MS),
  );
  const count = $derived(tasks.length);
  const runningCount = $derived(
    tasks.filter((t) => t.status === 'running').length,
  );
  // Top-level rows only (spec Q8): a nested launch's completion is its
  // parent agent's business — the pill must not pulse for it.
  const hasPendingCompletion = $derived(
    tasks.some((t) => t.depth === 0 && t.completion !== null),
  );

  return {
    get tasks() { return tasks; },
    get count() { return count; },
    get runningCount() { return runningCount; },
    get hasPendingCompletion() { return hasPendingCompletion; },
    get threadId() { return threadId; },
    get provider() { return provider; },

    mount(): () => void {
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
      const cancelBackgroundTaskState = wailsEventOn<BackgroundTaskStateEvent>(
        'provider:background_task_state',
        (evt) => {
          if (!evt || evt.threadId !== threadId) return;
          debouncedRefresh();
        },
      );
      return () => {
        cancelItemUpsert();
        cancelBackgroundTasksChanged();
        cancelBackgroundTaskState();
        debouncedRefresh.cancel();
      };
    },
  };
}
