// Background-tasks controller for the activity rail. Every row it shows is
// one `ListLiveBackgroundTasks` serves: it reads the list when its thread
// opens, after a reconnect or a lost frame, and when the backend asks, and
// otherwise applies `provider:background_tray` deltas
// (stores/eventsBackgroundTray.ts), which carry the rows of the launches
// that changed (internal/triage/background_tray.go). A Claude thread's tray
// therefore costs a delta per change, whatever the thread runs. A Codex
// thread's tray also lists runtime records no delta carries, so its frames
// ask for a read, as do its own row pushes. Reads go
// through a rate-bounded refresh (`utils/refreshScheduler`: a plain trailing
// debounce here starved forever under a live stream and left the pill
// showing a count nothing had refuted). It watches no agent scope and reads
// no child row. Exposes reactive `tasks` / `runningCount` for the rail's
// toggle pill and expanded body and `hasPendingCompletion` for the host's
// clock gate; a Claude agent's served run state goes to the shared registry
// (`stores/subagentRunState.svelte.ts`), which the rows read per launch.
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
import { onBackgroundTrayEvent } from '../../stores/eventsBackgroundTray';
import { onItemUpsert } from '../../stores/eventsItemStream';
import { wailsEventOn } from '../../stores/wailsEvents';
import { getTransportStatusFor, onBackendStatusChange } from '../../stores/transportStatus.svelte';
import { threadBackend } from '../../transport/entityIndex';
import { backendKeyForOrigin } from '../../transport/backends';
import { transportGapChannel, type TransportGap } from '../../transport/wsClient';
import type { BackgroundTrayEvent } from '../../types/events';
import type { Item } from '../../types/models';
import { asProviderID, type ProviderID } from '../../types/providers';
import { applyTrayDelta, deriveTrayTasks, type TrayTask } from '../../utils/backgroundTray';
import { createRefreshScheduler } from '../../utils/refreshScheduler';
import { createTrayLatestToolProjection } from '../../utils/codexTrayProjection';
import { subagentRunStateFromMeta, type SubagentRunState } from '../../utils/subagentRunState';
import { liveSubagentRunState, replaceSubagentRunStates } from '../../stores/subagentRunState.svelte';

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
  // Launch rows the snapshot lists, for deciding what an upsert changes.
  // Rebuilt with each wholesale write, never per event. A running row wins
  // over another row with its id, as in `deriveTrayTasks`.
  let listedLaunches = new Map<string, Item>();
  const latestTools = createTrayLatestToolProjection();

  // Deltas applied while a list read is in flight. The read may predate
  // them, so they are applied again over its answer, in arrival order: the
  // backend emits a thread's deltas in the order of the reads they carry.
  let inflightDeltas: BackgroundTrayEvent[] | null = null;

  // The listed launches' served run states go to the shared registry
  // (stores/subagentRunState.svelte.ts). A list read or a delta serves
  // them, so each write replaces the thread's set, which rewrites only the
  // launches whose state changed; a re-push, which carries a row's other
  // keys over, leaves it alone.
  function replaceBackgroundItems(forThreadId: string | null, items: Item[]): void {
    backgroundItems = items;
    listedLaunches = new Map();
    for (const item of items) {
      if (!item.completionOf && (item.status === 'running' || !listedLaunches.has(item.id))) {
        listedLaunches.set(item.id, item);
      }
    }
    if (forThreadId) {
      const states = new Map<string, SubagentRunState>();
      for (const [id, launch] of listedLaunches) {
        const state = subagentRunStateFromMeta(launch.meta);
        if (state) states.set(id, state);
      }
      replaceSubagentRunStates(forThreadId, states);
    }
    latestTools.reset(items);
  }

  function withDelta(items: readonly Item[], forThreadId: string, evt: BackgroundTrayEvent): Item[] {
    const rows = (evt.rows ?? []).filter((row) => row.threadId === forThreadId);
    return applyTrayDelta(items, evt.launchIds ?? [], rows, getNow(), COMPLETION_RETENTION_MS);
  }

  // A listed launch re-pushed while running carries its latest-tool
  // decoration onto its row in place. On a Claude thread that is all a
  // push does here: the deltas carry membership and served state. A Codex
  // thread's tray is read again when membership can change: a new
  // background launch or nested agent, a completion of a listed launch, or
  // a listed launch no longer running. A Codex agent's row is its runtime
  // record, not the settled spawn row pushed here, so that push changes
  // nothing. A child's ordinary tool_completion names no listed launch.
  function applyUpsert(item: Item): void {
    if (provider !== 'codex') {
      const listed = listedLaunches.get(item.id);
      if (listed !== undefined && !item.completionOf && item.status === 'running') {
        backgroundItems = latestTools.applyPushedLaunch(backgroundItems, item);
      }
      return;
    }
    if (item.completionOf) {
      if (item.isBackground || listedLaunches.has(item.completionOf)) refresh.request();
      return;
    }
    const listed = listedLaunches.get(item.id);
    if (listed === undefined) {
      const nestedCodexAgent = item.parentId && item.kind === 'tool_call' && item.toolName === 'collab_agent';
      if (item.isBackground || nestedCodexAgent) refresh.request();
      return;
    }
    if (listed.toolName === 'collab_agent') return;
    if (item.status !== 'running') {
      refresh.request();
      return;
    }
    backgroundItems = latestTools.applyPushedLaunch(backgroundItems, item);
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
        replaceBackgroundItems(null, []);
        return;
      }
      const owner = threadBackend(id);
      if (owner !== undefined && getTransportStatusFor(owner).status !== 'connected') return;
      const deltas: BackgroundTrayEvent[] = [];
      inflightDeltas = deltas;
      try {
        const items = (await ListLiveBackgroundTasks(id)) as Item[] | null;
        if (!token.isCurrent() || id !== threadId) return;
        let next = (items ?? []).filter((item) => item.threadId === id);
        for (const evt of deltas) next = withDelta(next, id, evt);
        replaceBackgroundItems(id, next);
      } catch (err) {
        if (!token.isCurrent() || id !== threadId) return;
        console.error('ActivityRail: ListLiveBackgroundTasks failed:', err);
        // A failed read says nothing about task lifetime. Keep the last
        // snapshot until a successful refresh or a switch to another thread.
      } finally {
        if (inflightDeltas === deltas) inflightDeltas = null;
      }
    },
  });

  // Refetch when the thread switches. Through the SAME scheduler as the event
  // refreshes, so the switch load cannot race one issued moments before it;
  // reset() is what makes the outgoing thread's in-flight answer stale.
  $effect(() => {
    replaceBackgroundItems(threadId, []);
    refresh.reset();
    refresh.request({ immediate: true });
  });

  const tasks = $derived<TrayTask[]>(
    deriveTrayTasks(backgroundItems, getNow(), COMPLETION_RETENTION_MS),
  );
  const count = $derived(tasks.length);
  // A parked agent (reported, waiting on its own background commands) is
  // live but not running: the pill's pulse and the body's running count
  // read only the rows doing work.
  const runningCount = $derived(
    tasks.filter((t) => t.status === 'running'
      && liveSubagentRunState(threadId, t.launch?.id)?.state !== 'parked').length,
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
        applyUpsert(item);
      });
      const cancelBackgroundTray = onBackgroundTrayEvent((evt) => {
        if (evt.threadId !== threadId) return;
        if (evt.refresh || provider === 'codex') {
          refresh.request();
          return;
        }
        inflightDeltas?.push(evt);
        replaceBackgroundItems(evt.threadId, withDelta(backgroundItems, evt.threadId, evt));
      });
      // Remote jobs have no timeline rows to incidentally repair this tray.
      // Recover its own snapshot when its computer reconnects or loses a
      // relevant event, using the same scheduler and ownership index as RPCs.
      const cancelStatus = onBackendStatusChange((backend, status) => {
        if (!threadId || threadBackend(threadId) !== backend) return;
        refresh.reset();
        if (status.status === 'connected') refresh.request({ immediate: true });
      });
      const cancelGap = wailsEventOn<TransportGap>(transportGapChannel, (gap, origin) => {
        if (!threadId || threadBackend(threadId) !== backendKeyForOrigin(origin.backendId)) return;
        if (gap?.channel !== 'provider:background_tray' && gap?.channel !== 'provider:item_event') return;
        // Every row this tray reads is its own thread's: a loss the
        // server attributed to other threads cost it nothing.
        if (gap.threads && !gap.threads.includes(threadId)) return;
        refresh.reset();
        refresh.request({ immediate: true });
      });
      return () => {
        cancelStatus();
        cancelGap();
        cancelItemUpsert();
        cancelBackgroundTray();
        refresh.dispose();
      };
    },
  };
}
