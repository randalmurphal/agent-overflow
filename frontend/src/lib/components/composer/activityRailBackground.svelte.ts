// Background-tasks controller for the activity rail. Owns the
// `ListLiveBackgroundTasks` polling, three event subscriptions
// (`provider:item_event`-derived `onItemUpsert`,
// `provider:background_tasks_changed`, `provider:background_task_state`),
// and the rate-bounded refresh they drive (`utils/refreshScheduler` — a plain
// trailing debounce here starved forever under a live stream and left the pill
// showing a count nothing had refuted). Exposes reactive `tasks` / `runningCount`
// for the rail's toggle pill and expanded body, and `hasPendingCompletion`
// for the host's clock gate.
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
import { getTransportStatusFor, onBackendStatusChange } from '../../stores/transportStatus.svelte';
import { threadBackend } from '../../transport/entityIndex';
import { backendKeyForOrigin } from '../../transport/backends';
import { transportGapChannel } from '../../transport/wsClient';
import type {
  BackgroundTaskStateEvent,
  BackgroundTasksChangedEvent,
} from '../../types/events';
import type { Item } from '../../types/models';
import { asProviderID, type ProviderID } from '../../types/providers';
import { deriveTrayTasks, type TrayTask } from '../../utils/backgroundTray';
import { createRefreshScheduler } from '../../utils/refreshScheduler';
import { createCodexLatestToolProjection } from '../../utils/codexTrayProjection';

// Brief retention so a completion has time to flicker into view as the
// terminal state but doesn't linger after the user has read it. Just
// long enough to register; not long enough to feel sticky.
const COMPLETION_RETENTION_MS = 200;
// The tray pill is an authoritative count read at a glance, and background
// activity arrives as an unbroken event stream while any pane streams — so the
// coalescing delay needs an absolute bound behind it. 100ms collapses a burst;
// 400ms is the longest the pill may disagree with the backend (it read 10 over
// a truth of 3-4 under the old trailing debounce, 2026-08-29) and caps a flood
// at a handful of list calls per second.
const REFRESH_DELAY_MS = 100;
const REFRESH_MAX_WAIT_MS = 400;

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
  // Raw: every writer replaces the snapshot, and rows are read, never
  // mutated in place, so a deep proxy per row would only add cost.
  let backgroundItems: Item[] = $state.raw([]);
  // Launch rows the snapshot lists, for deciding which upserts can change
  // it. Rebuilt with each wholesale write, never per event.
  let listedLaunches = new Set<string>();
  const codexTools = createCodexLatestToolProjection();

  function replaceBackgroundItems(items: Item[]): void {
    backgroundItems = items;
    listedLaunches = new Set();
    for (const item of items) {
      if (!item.completionOf) listedLaunches.add(item.id);
    }
    codexTools.reset(items);
  }

  // What can change the tray: a background launch or a background task's
  // terminal (both carry isBackground), or a write that settles a launch
  // the tray lists, through its completion sibling or in place (a launch
  // whose result cleared its background flag). A child's ordinary
  // tool_completion names a launch the tray does not list.
  function changesTray(item: Item): boolean {
    if (item.isBackground) return true;
    if (item.completionOf) return listedLaunches.has(item.completionOf);
    return item.status !== 'running' && listedLaunches.has(item.id);
  }

  // A draft pane's thread is a synthetic placeholder no computer owns.
  // There is nothing to read until it materializes, and asking would route
  // an id that no entity index can resolve.
  const threadId = $derived(getPane().hasDraftPlaceholder ? null : getPane().thread?.id ?? null);
  const provider = $derived(asProviderID(getPane().thread?.provider));

  // The scheduler owns staleness: its token flips false the moment a run is
  // superseded, the thread switches (reset) or the controller unmounts
  // (dispose), which is what the hand-rolled fetchSeq used to do — badly, since
  // it could not see a dispose. The thread-id comparison stays beside it: the
  // token answers "is this run still the live one", the id answers "is this
  // answer about the thread we are showing".
  const refresh = createRefreshScheduler({
    name: 'activityRailBackground',
    delayMs: REFRESH_DELAY_MS,
    maxWaitMs: REFRESH_MAX_WAIT_MS,
    run: async (token) => {
      const id = threadId;
      if (!id) {
        replaceBackgroundItems([]);
        return;
      }
      const owner = threadBackend(id);
      if (owner !== undefined && getTransportStatusFor(owner).status !== 'connected') return;
      try {
        const items = (await ListLiveBackgroundTasks(id)) as Item[] | null;
        if (!token.isCurrent() || id !== threadId) return;
        replaceBackgroundItems((items ?? []).filter((item) => item.threadId === id));
      } catch (err) {
        if (!token.isCurrent() || id !== threadId) return;
        console.error('ActivityRail: ListLiveBackgroundTasks failed:', err);
        // A failed read says nothing about task lifetime. Keep the last
        // snapshot until a successful refresh or a switch to another thread.
      }
    },
  });

  // Refetch when the thread switches. Through the SAME scheduler as the event
  // refreshes, so the switch load cannot race one issued moments before it;
  // reset() is what makes the outgoing thread's in-flight answer stale.
  $effect(() => {
    threadId;
    replaceBackgroundItems([]);
    refresh.reset();
    refresh.request({ immediate: true });
  });

  const tasks = $derived<TrayTask[]>(
    deriveTrayTasks(backgroundItems, getNow(), COMPLETION_RETENTION_MS),
  );
  const count = $derived(tasks.length);
  const runningCount = $derived(
    tasks.filter((t) => t.status === 'running').length,
  );
  // A settled pair prunes only when `getNow()` advances past its retention
  // window, and the host runs the shared clock off this flag. Every depth
  // counts: a nested completion that could not restart the clock sat in
  // `tasks` forever, inflating the pill until the tray was opened.
  const hasPendingCompletion = $derived(
    tasks.some((t) => t.completion !== null),
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
        if (provider === 'codex' && item.parentId && item.kind === 'tool_call') {
          if (item.toolName === 'collab_agent') {
            // A nested launch needs a store refresh so its own tray row joins
            // the hierarchy. Ordinary child tools can update the existing
            // tray projection directly and avoid a full recursive query per
            // tool call; reconnect hydration still comes from the store.
            refresh.request();
          } else {
            backgroundItems = codexTools.apply(backgroundItems, item);
          }
          return;
        }
        if (changesTray(item)) refresh.request();
      });
      const cancelBackgroundTasksChanged = wailsEventOn<BackgroundTasksChangedEvent>(
        'provider:background_tasks_changed',
        (evt) => {
          if (!evt || evt.threadId !== threadId) return;
          refresh.request();
        },
      );
      const cancelBackgroundTaskState = wailsEventOn<BackgroundTaskStateEvent>(
        'provider:background_task_state',
        (evt) => {
          if (!evt || evt.threadId !== threadId) return;
          refresh.request();
        },
      );
      // Remote jobs have no timeline rows to incidentally repair this tray.
      // Recover its own snapshot when its computer reconnects or loses a
      // relevant event, using the same scheduler and ownership index as RPCs.
      const cancelStatus = onBackendStatusChange((backend, status) => {
        if (!threadId || threadBackend(threadId) !== backend) return;
        refresh.reset();
        if (status.status === 'connected') refresh.request({ immediate: true });
      });
      const cancelGap = wailsEventOn<{ channel: string }>(transportGapChannel, (gap, origin) => {
        if (!threadId || threadBackend(threadId) !== backendKeyForOrigin(origin.backendId)) return;
        if (gap?.channel !== 'provider:item_event'
          && gap?.channel !== 'provider:background_tasks_changed'
          && gap?.channel !== 'provider:background_task_state') return;
        refresh.reset();
        refresh.request({ immediate: true });
      });
      return () => {
        cancelStatus();
        cancelGap();
        cancelItemUpsert();
        cancelBackgroundTasksChanged();
        cancelBackgroundTaskState();
        refresh.dispose();
      };
    },
  };
}
