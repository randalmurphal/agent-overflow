// Background-tasks controller for the activity rail. Owns the
// `ListLiveBackgroundTasks` polling, three event subscriptions
// (`provider:item_event`-derived `onItemUpsert`,
// `provider:background_tasks_changed`, `provider:background_task_state`),
// and the rate-bounded refresh they drive (`utils/refreshScheduler` — a plain
// trailing debounce here starved forever under a live stream and left the pill
// showing a count nothing had refuted). Exposes reactive `tasks` / `runningCount` /
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
import { createRefreshScheduler } from '../../utils/refreshScheduler';
import { parseJsonObject } from '../../utils/parseJsonObject';
import { CODEX_LATEST_TOOL_META } from '../../utils/codexTrayProjection';

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

function projectLatestCodexTool(items: Item[], tool: Item): Item[] {
  const parentId = tool.parentId?.trim();
  const summary = tool.summary.trim();
  if (!parentId || !summary || tool.toolName === 'collab_agent') return items;

  let changed = false;
  const projected = items.map((item) => {
    if (item.id !== parentId || item.toolName !== 'collab_agent' || item.status !== 'running') {
      return item;
    }
    const parsedMeta = parseJsonObject(item.meta);
    if (item.meta?.trim() && parsedMeta === null) {
      console.error(`ActivityRail: malformed Codex launch meta for ${item.id}`);
      return item;
    }
    const meta = parsedMeta ?? {};
    const currentTurnValue = meta[CODEX_LATEST_TOOL_META.turnIndex];
    const currentItemValue = meta[CODEX_LATEST_TOOL_META.itemIndex];
    const currentTurn = typeof currentTurnValue === 'number'
      ? currentTurnValue
      : -1;
    const currentItem = typeof currentItemValue === 'number'
      ? currentItemValue
      : -1;
    if (
      tool.turnIndex < currentTurn
      || (tool.turnIndex === currentTurn && tool.itemIndex < currentItem)
    ) {
      return item;
    }
    if (
      meta[CODEX_LATEST_TOOL_META.summary] === summary
      && tool.turnIndex === currentTurn
      && tool.itemIndex === currentItem
    ) return item;
    changed = true;
    return {
      ...item,
      meta: JSON.stringify({
        ...meta,
        [CODEX_LATEST_TOOL_META.summary]: summary,
        [CODEX_LATEST_TOOL_META.turnIndex]: tool.turnIndex,
        [CODEX_LATEST_TOOL_META.itemIndex]: tool.itemIndex,
      }),
    };
  });
  return changed ? projected : items;
}

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
        backgroundItems = [];
        return;
      }
      try {
        const items = (await ListLiveBackgroundTasks(id)) as Item[] | null;
        if (!token.isCurrent() || id !== threadId) return;
        backgroundItems = (items ?? []).filter((item) => item.threadId === id);
      } catch (err) {
        if (!token.isCurrent() || id !== threadId) return;
        console.error('ActivityRail: ListLiveBackgroundTasks failed:', err);
        backgroundItems = [];
      }
    },
  });

  // Refetch when the thread switches. Through the SAME scheduler as the event
  // refreshes, so the switch load cannot race one issued moments before it;
  // reset() is what makes the outgoing thread's in-flight answer stale.
  $effect(() => {
    threadId;
    backgroundItems = [];
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
        if (provider === 'codex' && item.parentId && item.kind === 'tool_call') {
          if (item.toolName === 'collab_agent') {
            // A nested launch needs a store refresh so its own tray row joins
            // the hierarchy. Ordinary child tools can update the existing
            // tray projection directly and avoid a full recursive query per
            // tool call; reconnect hydration still comes from the store.
            refresh.request();
          } else {
            backgroundItems = projectLatestCodexTool(backgroundItems, item);
          }
          return;
        }
        if (
          item.isBackground
          || item.completionOf
        ) {
          refresh.request();
        }
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
      return () => {
        cancelItemUpsert();
        cancelBackgroundTasksChanged();
        cancelBackgroundTaskState();
        refresh.dispose();
      };
    },
  };
}
