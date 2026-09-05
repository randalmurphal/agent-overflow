// stores/thread.svelte.ts
//
// `ThreadPane` — the composition root and the SOLE OWNER of per-thread
// runtime UI state (frontend/AGENTS.md § State ownership). Everything a pane
// knows is reachable from here; the helper modules below are pieces of this
// owner, each constructed once per pane and never shared between panes, not
// sibling stores with their own keys.
//
// What lives out of line, and where:
//   threadItemWindow.svelte.ts        the item window and every write to it
//   threadStreamingReveal.svelte.ts   smoothers + reveal gate + row-text chokepoint
//   threadTimelineWindow.svelte.ts    history cursors and the load methods
//   threadItemStreamApply.ts          the upsert/delta/meta/patch machine
//   threadSwitchLoad.svelte.ts        switch, sync, replica, cache pipeline
//   threadSubagentMemory.ts           fold registry, eviction, child hydration
//   threadRowUiState.svelte.ts        per-row expansion/attachment state
//   threadDraftPlaceholder.svelte.ts  the pre-materialization phase
//   threadPaneScroll.svelte.ts        controller slot, spring arming, scroll intent
//   threadPaneTurns.svelte.ts         turn start/settle/clear + the timeline facet
//   threadPaneCompanions.ts           plan / review / agent surfaces
//   threadPaneErrors.svelte.ts        the banner-stack error slots
//   threadChannelState / threadActivityRuns /
//   threadPendingInteractiveState / liveTodoState / threadLiveStateHydration
//
// Add per-thread runtime state to one of those (or a new one composed here),
// never to a store beside the pane.

import type { Item, Thread } from '../types/models';
import type {
  ApprovalRequest,
  ContextWindow,
  ItemDeltaEvent,
  ItemMetaEvent,
  ItemPatchEvent,
  TodoStep,
  ProviderSessionAccountEvent,
  ProviderStatusEvent,
  UserInputRequest,
} from '../types/events';
import type { ChannelMessage, ChannelStatePayload } from '../types/discussion';
import { getThreadById } from './threads.svelte';
import { refreshWatchedThreads } from './watchedThreads';
import { leaseDuringSettle } from '../utils/scrollLeaseDuringTransition';
import { createGitStatusView, type GitStatusView } from './gitStatusStore.svelte';
import { workspaceRefForThread } from '../utils/workspaceKey';
import type { WorkspaceRef } from '../types/git';
import {
  agentPaneRetainedRootScope,
  agentPaneScopeTrailHolds,
} from './agentPane.svelte';
import { collectAgentScopeRetainedIds } from './agentScopeView.svelte';
import type { RevealBoundary } from '../utils/subagentGrouping';
import type { SubagentFoldAggregate } from '../utils/subagentFold';
import { itemPayloadRetentionKey } from '../utils/rowUiRetention';
import type { ApplyItemUpsertsToWindowResult } from './threadItems';
import { createLiveTodoState } from './liveTodoState.svelte';
import { createThreadPendingInteractiveState } from './threadPendingInteractiveState.svelte';
import { createThreadActivityRuns } from './threadActivityRuns.svelte';
import { activityRunDefaultCollapsed, activityRunWindowRows } from './activityRunPrefs.svelte';
import type { TimelineTurnFacet } from './threadTurnProjection';
import { createThreadRowUiState, type RowUiStateRetention } from './threadRowUiState.svelte';
import { createThreadStreamingReveal } from './threadStreamingReveal.svelte';
import type { StreamingAssistantRenderContext } from './streamingAssistantReveal';
import { createThreadTimelineWindow } from './threadTimelineWindow.svelte';
import { createThreadSubagentMemory } from './threadSubagentMemory';
import { createThreadLiveStateHydration } from './threadLiveStateHydration';
import { createThreadSwitchLoad } from './threadSwitchLoad.svelte';
import { createThreadItemStreamApply } from './threadItemStreamApply';
import { createThreadItemWindow } from './threadItemWindow.svelte';
import { createThreadDraftPlaceholder } from './threadDraftPlaceholder.svelte';
import { createThreadPaneErrors } from './threadPaneErrors.svelte';
import { createThreadPaneScroll } from './threadPaneScroll.svelte';
import { createThreadPaneCompanions } from './threadPaneCompanions';
import { reviewSubjectForPane } from './reviewPane.svelte';
import { createThreadPaneTurns } from './threadPaneTurns.svelte';
import {
  normalizeContextWindowForThread,
  seedContextWindow,
} from './threadContextWindow';
import { createThreadChannelState } from './threadChannelState.svelte';
import {
  nowForLiveContent,
  type LoadOlderResult,
  type PaneScrollController,
  type ThreadPaneOptions,
} from './threadPaneShared';

// The only re-export left here is threadPaneShared's — the pane's OWN
// vocabulary, which this module is the composition root for.
//
// Do not add a convenience barrel for the other sub-factories.
// `ActiveTurn` lives in threadStatuses.svelte.ts,
// `parseTokenUsage`/`SettledTurn` in threadTurnProjection.ts, and the
// live-todo / activity-rail prefs in liveTodoState.svelte.ts — import
// them from there. Re-exporting them here made `threads.svelte.ts`
// reach the live-todo pref droppers through this module, which imports
// `threads.svelte.ts` back: an import cycle bought for nothing.
export {
  __setSmoothingClockForTest,
  paneWorkspacePath,
} from './threadPaneShared';
export type {
  DraftPlaceholderDefaults,
  DraftPlaceholderMode,
  DraftThreadPlaceholder,
  LoadOlderResult,
  PaneErrorKind,
  PaneScrollController,
  PreserveViewportBottomOptions,
} from './threadPaneShared';

/**
 * Creates a self-contained thread pane state instance.
 * Each pane tracks its own thread, unified timeline items, approvals,
 * context/banner state, and mode-specific UI. Components receive a
 * ThreadPane as a prop.
 */
export function createThreadPane(options: ThreadPaneOptions = {}) {
  const paneId = options.paneId ?? 'pane';
  let thread: Thread | null = $state(null);
  // THE one write to `thread`. Every path that puts a different thread (or
  // none) on this pane goes through here, and an identity change restates
  // the watched-thread set: that set is what admits the thread's
  // entity-filtered frames (`provider:item_event` above all) on this
  // connection, and it is composed from `pane.threadId` over the registry.
  // A draft placeholder contributes nothing to it, so the pane adopting the
  // real row on first send is exactly the moment the backend would otherwise
  // keep withholding the whole first turn (2026-09-03). Same-id replacements
  // (metadata patches) change nothing the set reads, and the transport dedups
  // the rest, so the recompute is keyed on identity alone.
  function assignThread(next: Thread | null): void {
    const before = thread?.id ?? null;
    thread = next;
    if ((next?.id ?? null) !== before) refreshWatchedThreads();
  }
  let contextWindow: ContextWindow | null = $state(null);
  let loading: boolean = $state(false);
  let showTerminal: boolean = $state(false);
  // sendInFlight is the optimistic stop-button gate. The composer flips
  // it true the moment the user clicks Send and clears it in `finally`.
  // Used by SendButton to render the stop variant before
  // `provider:turn_started` arrives, and by the thread.interrupt
  // keybinding's `when` clause so Esc clears the prompt during the
  // dispatch window. Cleared on thread switch in clear() so the pane
  // doesn't carry sending state into the next thread.
  let sendInFlight: boolean = $state(false);
  /**
   * Generation counter for switchThread. Incremented on every switchThread
   * entry so a slow paged fetch from thread A cannot clobber thread B's
   * items when the user flips between them quickly. Also exposed
   * publicly via the `switchGeneration` getter so MessageTimeline's
   * `$effect.pre` can detect same-thread re-switch (forced reloads
   * that mutate items in place) and re-run its restore reset path —
   * must be `$state` for that effect dependency to track.
   */
  let switchGeneration = $state(0);
  const optimisticItemIds = new Set<string>();
  // Non-reactive timestamp of the last LIVE timeline content advance — a
  // smoother reveal, an overwrite patch, a text-like provider row, a
  // visible-field update to an already mounted row (tool output preview,
  // running→completed result chrome; see events.ts
  // providerUpsertAdvancesLiveContent), or a wire append / reveal-gate
  // release entering the loaded tail (`armLiveContentAppendSpring`
  // in threadPaneScroll.svelte.ts — that path shares the arm's restore
  // gates, so a switch-load settle never stamps).
  // Read imperatively by the scroll controller (MessageTimeline's
  // `liveContentActive` getter) as a LIVENESS signal — it keeps the
  // spring's post-arrival sentinel alive and lets a composer resize ride
  // an in-flight glide. It does NOT choose spring vs sync-pin; growth
  // while pinned at the bottom always glides (see
  // utils/liveContentActivity.ts and utils/scroll/resolver.ts).
  // Deliberately NOT `$state`: it is stamped up to ~60×/sec during a
  // drain and is never read in a reactive scope, so `$state` would churn
  // every dependent derivation for no benefit.
  let lastLiveContentAt = 0;
  function stampLiveContent(): void {
    lastLiveContentAt = nowForLiveContent();
  }

  // The pane's user-facing error surface (one slot per kind, banner-stack
  // order, the newest-error read) lives in threadPaneErrors.svelte.ts.
  const paneErrors = createThreadPaneErrors();

  // The loaded item window and EVERY write to it — indexes, per-row signal
  // boxes, the two structural revisions, and the four commit chokepoints —
  // lives in threadItemWindow.svelte.ts. Its collaborators are all
  // constructed below (three of them take its commit entry points in their
  // own option bags), so they arrive as arrows it only calls from method
  // bodies. Destructured here because these are the pane's own working
  // vocabulary and several are handed to sub-factories by reference.
  const itemWindow = createThreadItemWindow({
    streamingReveal: () => streamingReveal,
    rowUiState: () => rowUiState,
    activityRuns: () => activityRuns,
    subagentMemory: () => subagentMemory,
    switchLoad: () => switchLoad,
  });
  const {
    getItems,
    getItemById,
    itemIndexById,
    writeItemAt,
    appendDirectAssistantLiteral,
    replaceTimelineItems,
    installTimelineItems,
    dropTimelineItems,
    commitUpsertResult,
  } = itemWindow;

  // Scroll-surface edge: the registered controller slot, the scroll-to-item
  // intent, and every structural-spring / warm-up arming decision. See
  // threadPaneScroll.svelte.ts.
  const paneScroll = createThreadPaneScroll({
    getThread: () => thread,
    getLoading: () => loading,
    getSwitchGeneration: () => switchGeneration,
    getItemCount: () => getItems().length,
    stampLiveContent,
  });
  const { armStructuralSpring, armLiveContentAppendSpring } = paneScroll;

  // The pre-materialization phase — the synthetic placeholder, its edits,
  // and the coalesced CreateThread — lives in
  // threadDraftPlaceholder.svelte.ts. `snapshotPaneForClose` and `clearPane`
  // are hoisted function declarations below, so "+ New" over a live thread
  // takes exactly the leaving-a-thread path a pane close does.
  const draftState = createThreadDraftPlaceholder({
    paneId,
    getThread: () => thread,
    setThread: assignThread,
    getItemCount: () => getItems().length,
    getShowTerminal: () => showTerminal,
    bumpSwitchGeneration: () => {
      switchGeneration++;
    },
    setContextWindow: (next) => {
      contextWindow = next;
    },
    setGeneralError: (message) => paneErrors.set(message, 'general'),
    snapshotForClose: snapshotPaneForClose,
    clearPane,
    switchLoad: () => switchLoad,
  });

  // The pane's thread IDENTITY as a $derived primitive. `thread` is
  // replaced wholesale by every thread:updated sync (mode toggle, title
  // regen, model change), and a plain getter over the $state has no
  // equality cutoff — every effect keyed on "which thread is mounted"
  // re-ran on each replacement even though the id never moved. The nav
  // rail's baseline effect was the visible casualty: shift+tab cleared
  // and refetched the whole-thread tick list, blinking the rail
  // (2026-08-19). A $derived does not propagate while its value is
  // unchanged, so identity consumers only wake when the pane actually
  // mounts a different thread.
  // ($derived.by, not $derived: the inline form typechecks at this spot,
  // where control flow has narrowed `thread` to its `null` initializer.)
  const stableThreadId = $derived.by(() =>
    draftState.placeholder ? null : (thread?.id ?? null),
  );
  // Same cutoff for the other primitive facts served off the thread
  // object: `terminalThreadId` keys a `{#key}` and the terminal
  // placement wiring, and `activeModel` (its $derived lives beside
  // `effectiveModel` below) is one effect away from the same trap. Every
  // primitive-valued getter over `thread` goes through a $derived, so no
  // consumer can be woken by a replacement that changed nothing it reads.
  const stableTerminalThreadId = $derived.by(() => thread?.id ?? null);
  // The CHECKOUT this pane's git affordances address, as the wire spells it.
  // Derived off the two primitive strings so the object's identity moves only
  // when one of them actually changes: consumers pass this straight into an
  // RPC argument and into the git-status attach effect, and a fresh object per
  // `thread` replacement (many per streamed turn) would re-subscribe the
  // workspace on every token. A draft placeholder carries both fields exactly
  // as a persisted row does, which is the whole point — no git RPC has to
  // resolve a directory out of a conversation.
  const workspaceProjectId = $derived.by(() => thread?.projectId ?? '');
  const workspaceDirPath = $derived.by(() => thread?.workspacePath ?? '');
  const workspaceRef = $derived.by<WorkspaceRef | null>(() =>
    workspaceRefForThread({
      projectId: workspaceProjectId,
      workspacePath: workspaceDirPath,
    }),
  );

  const rowUiState = createThreadRowUiState({
    getItemById,
    // Read at dispose time, after the caller has already replaced `items`
    // with the surviving window — so this IS the "still loaded" set.
    loadedPayloadRefs: getItems,
  });
  // Per-item smoother + reveal-gate machinery lives in
  // threadStreamingReveal.svelte.ts. Item-window commits finalize through
  // `finalizeItemsCommit` in threadItemWindow.svelte.ts, so no caller can
  // publish a new window while leaving the readable-drain boundary derived
  // from the old one.
  const streamingReveal = createThreadStreamingReveal({
    getItemById,
    getItemIndex: (itemId) => itemIndexById.get(itemId),
    getItems,
    setItemAt: writeItemAt,
    appendDirectAssistantLiteral,
    stampLiveContent,
    armStructuralSpring: armLiveContentAppendSpring,
    appendLivePayloadDeltaForItem: rowUiState.appendLivePayloadDeltaForItem,
  });
  // Windowed-history / paging machinery (loaded-window cursors and flags,
  // the prune paths, and the four load methods) lives in
  // threadTimelineWindow.svelte.ts. `subagentMemory` (owns child
  // hydration) is a `const` declared later — wrapping its call in an arrow
  // keeps the property read lazy (deferred until the arrow is actually
  // invoked, well after the whole closure finishes constructing), so it
  // never hits the TDZ; a direct `subagentMemory.hydrateChildren`
  // reference here would throw immediately instead.
  const timelineWindow = createThreadTimelineWindow({
    getItems,
    replaceTimelineItems,
    installTimelineItems,
    getThread: () => thread,
    getSwitchGeneration: () => switchGeneration,
    getScrollController: () => paneScroll.controller,
    hydrateSubagentChildren: (rootItemID) =>
      subagentMemory.hydrateChildren(rootItemID),
    // Prune cuts keep companion-rendered rows — see agentPaneHeldRowIds
    // (hoisted function declaration; called lazily, so the late
    // definition is safe, same as the subagentMemory arrow above).
    getHeldRowIds: () => agentPaneHeldRowIds(),
  });
  const pendingInteractiveState = createThreadPendingInteractiveState();
  // Activity-run registry: stable run identity across window edges, plus
  // collapse overrides and inner scroll/mount state. Session-only, matching
  // item-expansion leases; the durable layer is the user setting.
  const activityRuns = createThreadActivityRuns({
    defaultCollapsed: () => activityRunDefaultCollapsed(),
    windowRows: () => activityRunWindowRows(),
    scrollController: () => paneScroll.controller,
  });
  // Rate-limit snapshots live in the global `rateLimitsInfo.svelte.ts`
  // store keyed by provider and account — they are account cache state,
  // not thread state. Keeping them out of each pane means they survive
  // thread switches, turn completions, and metadata updates. The pane
  // only tracks which account its live provider session should select.
  let providerBanner: ProviderStatusEvent | null | undefined =
    $state(undefined);
  let providerSessionAccount: ProviderSessionAccountEvent | null =
    $state(null);
  let providerSessionAccountRevision = 0;
  function updateProviderSessionAccount(
    account: ProviderSessionAccountEvent | null,
  ): void {
    providerSessionAccount = account?.connected ? account : null;
    providerSessionAccountRevision += 1;
  }
  // The spinner-flash gate (`pastSpinnerThreshold` + its timer), the
  // in-flight live-arrival ledger, the window attestation and the
  // replica write-back timer live in threadSwitchLoad.svelte.ts as
  // `switchLoad`, which is their sole writer. The only read from out
  // here is `showLoadingSpinner`'s.

  // One-shot "focus the terminal once it exists" intent. Set by
  // runTerminalToggle on a drawer open (cold start) and by pane.focusLeft/Right
  // when navigating INTO an already-mounted terminal pane (warm start). It is
  // `$state` so the terminal surface can consume it reactively in a $effect:
  // the warm path mutates it on a live surface, which a plain closure `let`
  // (consumed once in onMount) would miss. The latch still survives the async
  // gap between "open requested" and "lazy drawer chunk loaded + mounted" — it
  // stays set until the surface mounts and reads it. Replaces the old
  // fire-once FOCUS_TERMINAL_EVENT, whose listener didn't exist yet when the
  // event fired on a cold first open (the lazy import hadn't resolved).
  let pendingTerminalFocus = $state(false);

  // Read view onto the shared, workspace-keyed git-status store —
  // GitActionsControl, the header diff/PR badges, the Ship Changes wizard,
  // and the review pane's staleness dot all read it. It resolves its key
  // from the pane's current thread on every read, so a thread switch or a
  // worktree move re-points it with no reset of its own: the incoming
  // thread's workspace answers immediately, and the outgoing one's entry is
  // released by whoever attached it. A draft placeholder resolves the same
  // way — its synthetic row carries the project and the directory. The
  // subscription itself is attached by ChatHeaderActions (see
  // gitStatusStore.svelte.ts).
  const gitStatus: GitStatusView = createGitStatusView(() => thread);

  const channelState = createThreadChannelState();
  // Plan sidebar / review pane / agent companion — which
  // is open for this pane and what opening each one means. See
  // threadPaneCompanions.ts.
  const companions = createThreadPaneCompanions({
    paneId,
    getThread: () => thread,
    getReviewSubject: () => reviewSubjectForPane({
      threadId: stableThreadId,
      thread,
      workspace: workspaceRef,
    }),
  });

  // Turn lifecycle (`latestSettledTurn`, the timeline turn facet, and the
  // four wire projections) lives in threadPaneTurns.svelte.ts.
  const paneTurns = createThreadPaneTurns({
    getThread: () => thread,
    settleRecentWindowPrune: () => timelineWindow.settleRecentWindowPrune(),
  });
  // Session-scoped model actually serving the thread after a provider
  // fallback. The durable thread.model remains the user's requested model.
  let effectiveModel = $state('');
  // See `stableThreadId` above: the equality cutoff between thread-object
  // replacement and consumers keyed on the model string.
  const stableActiveModel = $derived.by(() => effectiveModel || thread?.model || '');
  let effectiveModelRevision = 0;
  let effectiveModelBackendRevision = 0;
  function updateEffectiveModel(model: string): void {
    effectiveModel = model.trim();
    effectiveModelRevision += 1;
  }
  const liveTodoState = createLiveTodoState();
  // Thread live-state hydration protocol (GetThreadLiveState +
  // ListPendingInteractiveRequests fallback, projected onto the global
  // active-turn/send-queue registries and onto pendingInteractiveState /
  // liveTodoState) lives in threadLiveStateHydration.ts. Instantiated
  // here because both dependencies above must already exist.
  const liveStateHydration = createThreadLiveStateHydration({
    getThread: () => thread,
    getSwitchGeneration: () => switchGeneration,
    pendingInteractiveState,
    liveTodoState,
    getProviderSessionAccountRevision: () => providerSessionAccountRevision,
    hydrateProviderAccount: (account, expectedMutationRevision) => {
      if (providerSessionAccountRevision !== expectedMutationRevision) return;
      updateProviderSessionAccount(account);
    },
    getEffectiveModelRevision: () => effectiveModelRevision,
    hydrateEffectiveModel: (model, backendRevision, expectedMutationRevision) => {
      if (effectiveModelRevision !== expectedMutationRevision) return;
      if (backendRevision < effectiveModelBackendRevision) return;
      effectiveModelBackendRevision = backendRevision;
      updateEffectiveModel(model);
    },
    // Set-only: the live clears (thread-scoped `ready`, session
    // disconnect) stay event-owned. A snapshot without the versions says
    // nothing — clearing on it would race a `binary_stale` push that
    // landed while the RPC was in flight, and the backend only re-emits
    // on transitions, so a wrongly cleared banner would never come back.
    hydrateBinaryStaleBanner: (sessionVersion, installedVersion) => {
      if (!thread || !sessionVersion || !installedVersion) return;
      providerBanner = {
        provider: thread.provider,
        status: 'binary_stale',
        threadId: thread.id,
        sessionVersion,
        installedVersion,
        actionable: true,
      };
    },
  });

  /**
   * Every row the OPEN agent companion is rendering (the scope trail's
   * whole subtree), or null when no pane is open. Shared by the two
   * chokepoints that can remove rows from pane memory — the fold
   * eviction commit and the timeline window's prune cuts — because a
   * row disappearing out from under the mounted companion blanks the
   * very transcript the reader opened, whichever path dropped it.
   */
  function agentPaneHeldRowIds(): ReadonlySet<string> | null {
    const rootScope = thread !== null ? agentPaneRetainedRootScope(paneId, thread.id) : '';
    return rootScope ? collectAgentScopeRetainedIds(getItems(), rootScope) : null;
  }

  // Subagent transcript-memory domain (the live-eviction fold registry,
  // settled-child eviction policy, and on-demand child hydration) lives
  // in threadSubagentMemory.ts.
  const subagentMemory = createThreadSubagentMemory({
    getItems,
    getItemIndex: (itemId) => itemIndexById.get(itemId),
    replaceTimelineItems,
    dropTimelineItems,
    getThread: () => thread,
    getSwitchGeneration: () => switchGeneration,
    // An anchor the OPEN agent pane is scoped to (or holds on its trail)
    // retains its children exactly like an expanded card: the pane is a
    // live view of those rows, so folding them out from under it would
    // blank the very transcript the reader opened.
    isSubagentGroupExpanded: (groupKey: string) =>
      rowUiState.isSubagentGroupExpanded(groupKey) ||
      (thread !== null && agentPaneScopeTrailHolds(paneId, thread.id, groupKey)),
    // The commit-chokepoint half of the same rule: eviction paths that
    // never consult per-anchor expansion (collapse-time eviction, the
    // collapsed-launch subtree sweep) still must not fold rows the open
    // pane is rendering.
    agentPaneHeldRows: agentPaneHeldRowIds,
  });

  /**
   * Row-UI retention union for the open agent pane (the row-UI-state
   * half of the retention rule above): the scope trail's whole subtree
   * — item ids, their payload keys, and their group keys — joins
   * whatever the chat timeline's prune pass retained. No open pane, no
   * cost: the original retention passes through untouched.
   */
  function widenRetentionForAgentPane(retention: RowUiStateRetention): RowUiStateRetention {
    const rootScope = thread !== null ? agentPaneRetainedRootScope(paneId, thread.id) : '';
    if (!rootScope) return retention;
    const scopeIds = collectAgentScopeRetainedIds(getItems(), rootScope);
    if (scopeIds.size === 0) return retention;
    const itemIds = new Set(retention.itemIds);
    const groupKeys = new Set(retention.groupKeys);
    const payloads = new Set(retention.payloads);
    for (const id of scopeIds) {
      groupKeys.add(id);
      if (itemIds.has(id)) continue;
      itemIds.add(id);
      const item = getItemById(id);
      if (!item) continue;
      const payloadKey = itemPayloadRetentionKey(item);
      if (payloadKey !== null) payloads.add(payloadKey);
    }
    return { itemIds, payloads, groupKeys };
  }

  // Subagent eviction policy (evictableAnchorIdFor, collectSettledSubtree,
  // commitSubagentEvictions, evictSettledChildren, evictCollapsedSubtree)
  // lives in threadSubagentMemory.ts as `subagentMemory`.
  // The per-item smoother + reveal-gate sequencer (disposeSmootherFor,
  // disposeAll, recomputeReveal, getOrCreateSmoothing, etc.) live in
  // threadStreamingReveal.svelte.ts as `streamingReveal`. Both item-window
  // commit chokepoints finalize the reveal gate internally, after all domain
  // work that can change the final window or smoother set.

  // The streaming item-application machine (upsertItemsBatch and the
  // applyProviderItemUpserts / applyItemDelta / applyItemMeta /
  // applyItemPatch bodies) lives in threadItemStreamApply.ts as
  // `itemStream`; the thread-switch / window-sync / replica pipeline
  // (including switchThread and refreshFromBackend) lives in
  // threadSwitchLoad.svelte.ts as `switchLoad`. Both are constructed
  // below the last state they capture.

  // Thread live-state hydration protocol (applyPendingInteractiveSnapshot,
  // applyThreadLiveStateSnapshot, startLiveStateFetch) lives in
  // threadLiveStateHydration.ts as `liveStateHydration`.

  // Child-transcript hydration for a subagent launch anchor
  // (hydrateSubagentChildren) lives in threadSubagentMemory.ts as
  // `subagentMemory.hydrateChildren`.

  // Shared removal core for every path that takes rows OUT of the
  // timeline on purpose (removeItemsFromTurn / removeRevertedItems /
  // removeItemById). The drop itself and the disposal that follows it
  // belong to `dropTimelineItems` → `disposeDroppedItemState`; what is
  // left here is evicting the warm-re-entry cache so a thread-switch restore cannot
  // resurrect rows the user just destroyed.
  //
  // Routing through the chokepoint is also a behavior fix. Hand-rolling
  // the disposal skipped `subagentMemory.resetHydrationExhausted`, and a
  // removal can drop hydrated subagent children while their launch
  // anchor SURVIVES: `removeRevertedItems` keeps the anchor turn's
  // backend-enumerated survivors and drops everything else on that turn,
  // and `removeItemById` drops exactly one row. A surviving anchor still
  // marked exhausted never re-fetches, so the card wedges on its loading
  // placeholder until the thread is switched away and back.
  //
  // Returns the removed items in their previous order; [] when nothing
  // matched (idempotent).
  function removeMatchedItems(shouldRemove: (item: Item) => boolean): Item[] {
    // No `exhaustedScope`: mapping a dropped grandchild back to its launch
    // root would need an ancestor walk over rows we just dropped, and a
    // truncation is exactly the bulk case `resetHydrationExhausted`
    // documents as clearing wholesale.
    const removed = dropTimelineItems(shouldRemove);
    if (removed.length === 0) return removed;
    if (thread) switchLoad.dropCachedWindow(thread.id);
    return removed;
  }

  // Thread-switch / window-sync / replica pipeline. Declared last
  // because the ctx captures the sub-factory handles BY VALUE — pane
  // fields it only reads through getters could be declared after it,
  // but a handle could not. Nothing calls into it during construction.
  const switchLoad = createThreadSwitchLoad({
    paneId,
    getThread: () => thread,
    setThread: assignThread,
    getDraftPlaceholder: () => draftState.placeholder,
    clearDraftPlaceholder: draftState.reset,
    getItems,
    installTimelineItems,
    getSwitchGeneration: () => switchGeneration,
    bumpSwitchGeneration: () => ++switchGeneration,
    getLoading: () => loading,
    setLoading: (value) => {
      loading = value;
    },
    resetLiveContentStamp: () => {
      lastLiveContentAt = 0;
    },
    getScrollController: () => paneScroll.controller,
    armInitialSliceWarmup: paneScroll.armInitialSliceWarmup,
    getLatestSettledTurn: () => paneTurns.latestSettledTurn,
    setLatestSettledTurn: paneTurns.setLatestSettledTurn,
    getContextWindow: () => contextWindow,
    setContextWindow: (next) => {
      contextWindow = next;
    },
    setPaneError: paneErrors.set,
    clearPaneError: paneErrors.clear,
    setProviderBanner: (status) => {
      providerBanner = status;
    },
    setProviderSessionAccount: updateProviderSessionAccount,
    setSendInFlight: (value) => {
      sendInFlight = value;
    },
    getShowTerminal: () => showTerminal,
    setShowTerminal: (value) => {
      showTerminal = value;
    },
    setEffectiveModel: updateEffectiveModel,
    optimisticItemIds,
    invalidatedDraftTerminalIds: draftState.invalidatedDraftTerminalIds,
    timelineWindow,
    subagentMemory,
    rowUiState,
    activityRuns,
    streamingReveal,
    channelState,
    pendingInteractiveState,
    liveTodoState,
    liveStateHydration,
  });

  // Streaming item-application machine. Same construction rule as
  // `switchLoad` above, so it is declared after it.
  const itemStream = createThreadItemStreamApply({
    getItems,
    itemIndexById,
    getThread: () => thread,
    writeItemAt,
    commitUpsertResult,
    stampLiveContent,
    armLiveContentAppendSpring,
    optimisticItemIds,
    timelineWindow,
    subagentMemory,
    streamingReveal,
  });

  /**
   * Pane-close counterpart of the thread-switch snapshot: cache the
   * item window (+ durable replica, size priors) so a later reopen is
   * a warm restore, not a cold fetch with an estimate→measure spring
   * (bug-report-20260822T020840Z). Called by `destroyPane` BEFORE
   * `clear()` empties the items, and by `startDraftPlaceholder` for the
   * same leaving-the-thread edge. Skips a thread the store no longer
   * lists — deletion flows call `removeThread` (which evicts every
   * cache tier) before closing the panes, and caching here would
   * resurrect the just-evicted window.
   */
  function snapshotPaneForClose(): void {
    if (!thread || !getThreadById(thread.id)) return;
    switchLoad.snapshotPaneForClose();
  }

  function clearPane(): void {
    // Any intent staged against the (about-to-be-discarded) placeholder
    // id must die with it — see threadDraftPlaceholder.svelte.ts
    // `closePlaceholderIntents`.
    draftState.closePlaceholderIntents();
    companions.closeAll();
    // Dispose before severing the thread/window pair. disposeAll clears its
    // state before reporting callback failures, so an aborted clear leaves a
    // coherent settled pane that can be cleared again.
    streamingReveal.disposeAll();
    assignThread(null);
    updateEffectiveModel('');
    draftState.reset();
    replaceTimelineItems([]);
    subagentMemory.clearFolds();
    rowUiState.clear();
    activityRuns.clear();
    // Clearing to empty: drop the live-content stamp too (see
    // installCacheOrFreshState — keeps a stale stamp from springing the
    // next thread's settled content). The switchGeneration bump below
    // also cancels any in-flight structural nudge.
    lastLiveContentAt = 0;
    pendingInteractiveState.clear();
    contextWindow = null;
    providerBanner = undefined;
    updateProviderSessionAccount(null);
    paneErrors.clear();
    loading = false;
    sendInFlight = false;
    optimisticItemIds.clear();
    showTerminal = false;
    channelState.clear();
    // activeTurn lives in the global registry (threadStatuses) and is
    // cleared by projectTurnCompleted; clearing it from a pane.clear()
    // would race with an in-flight turn on the same thread that
    // belongs to a different pane. The pane's getter just stops
    // returning a value once thread is null below.
    paneTurns.setLatestSettledTurn(null);
    liveTodoState.resetForEmptyPane();
    // Same shape for everything the switch/sync pipeline holds against
    // the thread we just dropped — the spinner-threshold timer, the
    // replica write-back timer, the window attestation and the
    // in-flight live-arrival ledger. See `resetPipeline` in
    // threadSwitchLoad.svelte.ts for what each one would otherwise
    // leak into the next thread.
    switchLoad.resetPipeline();
    timelineWindow.resetForFreshThread();
    subagentMemory.clearWindowDerivedState();
    // See switchThread: both `timelineWindow`'s internal
    // `pagingGeneration` and `scrollToItemRequest.nonce` stay
    // monotonic for the pane's lifetime so no consumer observes a
    // regressed counter.
    // Git status needs no reset: it is keyed by workspace in a shared
    // store, so clearing the pane's thread already re-points the view at
    // "no workspace".
    // Invalidate any in-flight switchThread so its late resolutions can't
    // repopulate the pane we just cleared.
    switchGeneration++;
  }

  return {
    // --- Getters (reactive reads) ---
    get paneId() {
      return paneId;
    },
    get thread() {
      return thread;
    },
    get threadId() {
      return stableThreadId;
    },
    /**
     * The checkout this pane operates in, or null when the row names none
     * (a terminal-only thread has no project). Every workspace-scoped git
     * RPC takes this value; a surface whose action cannot run without it
     * does not render rather than rendering and ignoring the click.
     */
    get workspace() {
      return workspaceRef;
    },
    /**
     * Key the timeline's scroll state (snapshots, restore identity) is
     * stored under. The base pane's is its thread id; a scoped facade
     * (agentScopeView) overrides it per scope so an agent pane's scroll
     * position never clobbers the main timeline's saved position for the
     * same thread. timelineRestore must key on THIS, never on threadId.
     */
    get scrollStateKey() {
      return stableThreadId;
    },
    // Empty on the thread timeline. Agent-scope facades override this so a
    // nested launch renders as a navigation edge instead of an inline body
    // whose descendants deliberately belong to another scope.
    get agentScopeRootId() {
      return '';
    },
    get activeModel() {
      return stableActiveModel;
    },
    get effectiveModel() {
      return effectiveModel;
    },
    get terminalThreadId() {
      return stableTerminalThreadId;
    },
    get draftPlaceholder() {
      return draftState.placeholder;
    },
    get hasDraftPlaceholder() {
      return draftState.placeholder !== null;
    },
    get canCompose() {
      return Boolean(thread || draftState.placeholder);
    },
    get items() {
      return getItems();
    },
    // Imperative read for the scroll controller's content-keyed spring
    // latch. Non-reactive on purpose (see the `lastLiveContentAt`
    // declaration); callers must read it inside an imperative context,
    // not a `$derived`/`$effect`.
    get lastLiveContentAt() {
      return lastLiveContentAt;
    },
    // Stamp a live content advance from a site OUTSIDE the pane's own
    // mutation methods — specifically the live provider-upsert fan-out in
    // events.ts (a new row arriving). The optimistic user-send echo and
    // rollback-restore call `upsertItems` directly and intentionally do
    // NOT route through here, so they stay sync-pinned.
    markLiveContentAdvanced: stampLiveContent,
    setDraftPlaceholderMode: draftState.setDraftPlaceholderMode,
    applyDraftPlaceholderDefaults: draftState.applyDraftPlaceholderDefaults,
    applyDraftPlaceholderWorkspace: draftState.applyDraftPlaceholderWorkspace,
    dematerializeEmptyDraftThread: draftState.dematerializeEmptyDraftThread,
    /**
     * "Locked in" — the user has sent at least one message, so the
     * provider/model selection is committed for this thread. UI
     * affordances that should hide while the thread is still in its
     * pre-send configuration phase (rate-limit rings, model picker
     * disable) read this getter rather than re-deriving from
     * `items.length`.
     */
    get isLocked() {
      return getItems().length > 0;
    },
    get timelineRevision() {
      return itemWindow.timelineRevision;
    },
    get rowUiRetentionRevision() {
      return itemWindow.rowUiRetentionRevision;
    },
    getItemById,
    get pendingApprovals() {
      return pendingInteractiveState.approvals;
    },
    get pendingUserInputs() {
      return pendingInteractiveState.userInputs;
    },
    get contextWindow() {
      return contextWindow;
    },
    get providerBanner() {
      return providerBanner;
    },
    get providerSessionAccount() {
      return providerSessionAccount;
    },
    /**
     * Every stored pane error in banner-stack order; each renders as its
     * own row with its own action and Dismiss. See threadPaneErrors.
     */
    get paneErrorList() {
      return paneErrors.list();
    },
    /** Newest stored error's message; presence-check convenience. */
    get generalError() {
      return paneErrors.newest()?.message ?? null;
    },
    /**
     * Tag of the message above. `'general'` reports as `null` — an
     * untagged error has no affordance, which is the distinction this
     * getter has always drawn.
     */
    get generalErrorKind() {
      const kind = paneErrors.newest()?.kind ?? null;
      return kind === 'general' ? null : kind;
    },
    get loading() {
      return loading;
    },
    /**
     * Spinner-flash gate. The MessageTimeline reads this instead of
     * `loading` so a sub-100ms switch (cache hit, fast LAN, fast SQL)
     * never shows the spinner — the view transitions straight to the
     * loaded content. Above the threshold the spinner fades in. See
     * `SPINNER_THRESHOLD_MS`.
     */
    get showLoadingSpinner() {
      // Items present is the second half of the gate: a cache hit paints
      // synchronously even while the recent-turns / live-state fetches
      // still run (loading=true), and we must not flash a spinner over
      // visible content. Single source of truth here so call sites
      // stay simple.
      return loading && switchLoad.pastSpinnerThreshold && getItems().length === 0;
    },
    /**
     * True between the moment the user clicks Send and the moment
     * SendMessage resolves (success or failure). The composer uses
     * this to render the optimistic stop button before
     * `provider:turn_started` lands; the keybindings dispatcher uses
     * it to enable Esc → thread.interrupt during the same window.
     */
    get sendInFlight() {
      return sendInFlight;
    },
    get showTerminal() {
      return showTerminal;
    },
    get gitStatus() {
      return gitStatus;
    },
    canAdoptOpenedTerminal: draftState.canAdoptOpenedTerminal,
    /**
     * Most recent completed turn, or null if the thread has no settled
     * turns yet. Populated from `provider:turn_completed` pushes and
     * from thread-switch rehydration. See threadPaneTurns.svelte.ts.
     */
    get latestSettledTurn() {
      return paneTurns.latestSettledTurn;
    },
    get timelineTurns(): TimelineTurnFacet {
      return paneTurns.timelineTurns;
    },
    /**
     * Inclusive floor of the loaded history window. Consumers use this
     * to render "Load older messages" and, in scroll-to-item flows, to
     * decide whether a target coordinate is already in view.
     */
    get oldestLoadedCursor() {
      return timelineWindow.oldestLoadedCursor;
    },
    get newestLoadedCursor() {
      return timelineWindow.newestLoadedCursor;
    },
    get oldestLoadedTurnIndex() {
      return timelineWindow.oldestLoadedTurnIndex;
    },
    get newestLoadedTurnIndex() {
      return timelineWindow.newestLoadedTurnIndex;
    },
    get hasMoreHistory() {
      return timelineWindow.hasMoreHistory;
    },
    get hasMoreNewer() {
      return timelineWindow.hasMoreNewer;
    },
    get hasDeferredRecentWindowPrune() {
      return timelineWindow.hasDeferredRecentWindowPrune;
    },
    retryDeferredRecentWindowPrune(): void {
      timelineWindow.retryDeferredRecentWindowPrune();
    },
    get loadingOlder() {
      return timelineWindow.loadingOlder;
    },
    get loadingNewer() {
      return timelineWindow.loadingNewer;
    },
    debugMemoryStats() {
      const streamingStats = streamingReveal.debugStats();
      return {
        itemIndexEntries: itemIndexById.size,
        rowUiState: rowUiState.debugStats(),
        itemSmoothers: streamingStats.itemSmoothers,
        liveThinkingTails: streamingStats.liveThinkingTails,
        liveThinkingTailChars: streamingStats.liveThinkingTailChars,
        optimisticItems: optimisticItemIds.size,
        oldestLoadedCursor: timelineWindow.oldestLoadedCursor,
        newestLoadedCursor: timelineWindow.newestLoadedCursor,
      };
    },
    /**
     * Scroll-to-item intent published by pane-level callers (search
     * hits, plan sidebar clicks, tray rows). MessageTimeline reacts to
     * nonce changes — the timeline compares the observed nonce against
     * the current value and runs `scrollToItem(itemId)` when it
     * advances. `itemId === ''` means "no request".
     */
    get scrollToItemRequest() {
      return paneScroll.scrollToItemRequest;
    },
    get channelMessages() {
      return channelState.messages;
    },
    get channelStatus() {
      return channelState.status;
    },
    get channelTurnCount() {
      return channelState.turnCount;
    },
    get channelMaxTurns() {
      return channelState.maxTurns;
    },
    get channelAwaitingResponse() {
      return channelState.awaitingResponse;
    },
    get channelCurrentSpeakerRole() {
      return channelState.currentSpeakerRole;
    },
    get channelParticipants() {
      return channelState.participants;
    },
    get channelLiveTail() {
      return channelState.liveTail;
    },
    /**
     * Non-reactive `performance.now()` stamp of the last live discussion
     * advance (a new channel message, or live-tail growth). Read
     * imperatively by ChannelView's scroll controller
     * `liveContentActive` — mirrors `pane.lastLiveContentAt`'s
     * chat-surface role. See
     * `threadChannelState.svelte.ts`.
     */
    get channelLastLiveContentAt() {
      return channelState.lastLiveContentAt;
    },
    // Companion surfaces — see threadPaneCompanions.ts.
    get showPlanSidebar() {
      return companions.showPlanSidebar;
    },
    get showReviewPane() {
      return companions.showReviewPane;
    },
    get showAgentPane() {
      return companions.showAgentPane;
    },
    /**
     * Monotonically increasing counter bumped at the top of every
     * `switchThread`, `clear`, `startDraftPlaceholder`, and
     * `adoptMaterializedDraftThread` call. Exposed so consumers can
     * detect a same-thread re-switch — the path a forced
     * `switchThread(currentThread)` reload takes to replace items
     * in place. `pane.threadId` doesn't change on that path, so
     * any reset logic keyed purely on the thread id (the
     * MessageTimeline restore-effect.pre, in particular) would miss the
     * event and leave stale scroll state (the regression: revert lands
     * at the very top, showing "Load older messages"). Track this
     * alongside `pane.threadId` and run the reset branch when EITHER
     * value changes.
     */
    get switchGeneration() {
      return switchGeneration;
    },

    // --- Thread switching ---

    /**
     * Point the pane at `newThread`. The whole pipeline — outgoing
     * snapshot, incoming reset, cache-or-replica paint, the
     * `SyncThreadWindow` convergence and the parallel hydration
     * fan-out — lives in threadSwitchLoad.svelte.ts.
     */
    switchThread(newThread: Thread): Promise<void> {
      return switchLoad.switchThread(newThread);
    },

    /**
     * Re-fetch the visible window from the backend without resetting
     * pane-scoped UI state (terminal / diff panel / draft). Used by the
     * transport-gap consumer when a missed event window forces a full
     * reconcile of the active pane. See threadSwitchLoad.svelte.ts.
     */
    refreshFromBackend(): Promise<void> {
      return switchLoad.refreshFromBackend();
    },

    retryHistoryLoad(): Promise<void> {
      return switchLoad.retryHistoryLoad();
    },

    snapshotForClose: snapshotPaneForClose,

    clear: clearPane,

    startDraftPlaceholder: draftState.startDraftPlaceholder,

    materializeDraftPlaceholder: draftState.materializeDraftPlaceholder,

    adoptMaterializedDraftThread: draftState.adoptMaterializedDraftThread,

    ensureMaterializedThread: draftState.ensureMaterializedThread,

    /** Fetch the next batch of older turns and prepend them to the window. See threadTimelineWindow.svelte.ts. */
    loadOlder(): Promise<LoadOlderResult> {
      return timelineWindow.loadOlder();
    },

    /** Ensure `itemID` is present in the loaded window. See threadTimelineWindow.svelte.ts. */
    loadUntilItem(itemID: string): Promise<boolean> {
      return timelineWindow.loadUntilItem(itemID);
    },

    /**
     * Hydrate the child transcript under a subagent launch anchor —
     * called by SubagentGroup when an expanded card's loaded children
     * trail its decorated descendant count. Deduped per anchor id;
     * see threadSubagentMemory.ts `hydrateChildren`.
     */
    ensureSubagentChildren(rootItemID: string): Promise<boolean> {
      return subagentMemory.hydrateChildren(rootItemID);
    },

    /** Fetch the next batch of newer turns and append them to the window. See threadTimelineWindow.svelte.ts. */
    loadNewer(): Promise<LoadOlderResult> {
      return timelineWindow.loadNewer();
    },

    /** Reload the tail slice around the thread's most recent item. See threadTimelineWindow.svelte.ts. */
    loadRecentTail(): Promise<boolean> {
      return timelineWindow.loadRecentTail();
    },

    requestScrollToItem: paneScroll.requestScrollToItem,

    /**
     * Registered scroll controller for this pane. Read by surfaces that
     * need to suspend auto-follow during a gesture. Call
     * `pause = pane.scrollController?.pauseAutoScroll()`
     * on pointerdown and `pause?.()` on pointerup/cancel — the lease is
     * idempotent so a stray double-release is safe.
     */
    get scrollController(): PaneScrollController | null {
      return paneScroll.controller;
    },

    /** MessageTimeline calls this on mount; clears on destroy. */
    attachScrollController: paneScroll.attach,

    detachScrollController: paneScroll.detach,

    // --- Mutations (called by event router) ---

    addApproval(approval: ApprovalRequest): void {
      pendingInteractiveState.addApproval(approval);
    },

    removeApproval(requestId: string): void {
      pendingInteractiveState.removeApproval(requestId);
    },

    addUserInput(request: UserInputRequest): void {
      pendingInteractiveState.addUserInput(request);
    },

    removeUserInput(requestId: string): void {
      pendingInteractiveState.removeUserInput(requestId);
    },

    /**
     * One-item compatibility wrapper around the batched upsert path.
     * Neither this nor `upsertItems` arms the structural spring: wire
     * appends route through `applyProviderItemUpserts` below, which
     * does. The un-armed paths are for rows that must NOT animate the
     * viewport (revert-on-interrupt restores) or arm at their own call
     * site (the composer's optimistic send).
     */
    upsertItem(item: Item): boolean {
      return itemStream.upsertItemsBatch([item]) !== null;
    },

    /**
     * Merge a batch of Items from `provider:item_event` into the timeline.
     * The final state is still the backend-authored transcript, but bursts
     * only allocate/sort/bump revision once.
     */
    upsertItems(incoming: Item[]): boolean {
      return itemStream.upsertItemsBatch(incoming) !== null;
    },

    /**
     * Provider event fan-out needs the applied changed rows, not just a
     * boolean, so scroll latches are based on visible-window changes after
     * the pane has filtered below-floor history rows. See
     * threadItemStreamApply.ts.
     */
    applyProviderItemUpserts(
      incoming: Item[],
    ): ApplyItemUpsertsToWindowResult | null {
      return itemStream.applyProviderItemUpserts(incoming);
    },

    /**
     * Remove a single item from the pane's timeline by id. Returns the
     * removed Item so optimistic callers (revert-on-interrupt) can
     * re-insert it on rollback. Idempotent: returns null when the row
     * is already gone, so a late `user_message:reverted` event after
     * the optimistic remove is a no-op.
     *
     * `expectedThreadId` is required rather than optional because every
     * caller reaches here across an await or an event hop, and the pane
     * may have moved on: item ids are per-thread (`user:<n>` collides
     * across threads by construction), and the removal also drops the
     * thread's cached window. Naming the thread is the enforcement — a
     * caller cannot reach the removal without saying which conversation
     * it believes it is editing, and a mismatch is a no-op.
     */
    removeItemById(itemId: string, expectedThreadId: string): Item | null {
      if (!thread || thread.id !== expectedThreadId) return null;
      // The index answers "is it here?" in O(1); the removal itself goes
      // through the same core as the truncation paths so a one-row drop
      // cannot diverge from a bulk one. The predicate walk replaces the
      // `items.filter` this used to do — the same single pass, now
      // yielding the dropped row instead of discarding it.
      if (!itemIndexById.has(itemId)) return null;
      const [removed] = removeMatchedItems((it) => it.id === itemId);
      return removed ?? null;
    },

    /**
     * Remove every item with `turnIndex >= fromTurnIndex` from the
     * pane's timeline. Mirrors the backend `DeleteConversationFromTurn`
     * truncate that revert-on-interrupt and explicit revert run under
     * the thread lock — only `user_message:reverted` notifies the user
     * row, so synthetic siblings on the same turn (thinking, api_retry,
     * error, notification, terminal_interaction waits) would otherwise
     * strand in the timeline without backing SQLite rows.
     *
     * Returns the removed items in their previous order so optimistic
     * callers can restore them via `upsertItems` on rollback (the
     * plain-interrupt fallback when the backend predicate disagrees).
     * Idempotent: returns `[]` when no rows match.
     */
    removeItemsFromTurn(fromTurnIndex: number): Item[] {
      if (!Number.isFinite(fromTurnIndex)) return [];
      return removeMatchedItems((it) => it.turnIndex >= fromTurnIndex);
    },

    /**
     * Mirror a backend conversation revert exactly: remove every item
     * with `turnIndex > turnIndex`, and within the anchor turn itself
     * remove everything NOT named in `keptAnchorTurnItemIds` — the
     * survivor list the `user_message:reverted` event carries from
     * `DeleteConversationFromItem`. The kept-set formulation (rather
     * than a removed-set) is load-bearing: pane-only rows that were
     * never persisted are absent from any backend enumeration, and a
     * kept-set removes them for free. An empty list degenerates to
     * `removeItemsFromTurn(turnIndex)`. Idempotent like its sibling.
     */
    removeRevertedItems(turnIndex: number, keptAnchorTurnItemIds: string[]): Item[] {
      if (!Number.isFinite(turnIndex)) return [];
      if (keptAnchorTurnItemIds.length === 0) {
        return removeMatchedItems((it) => it.turnIndex >= turnIndex);
      }
      const kept = new Set(keptAnchorTurnItemIds);
      return removeMatchedItems(
        (it) => it.turnIndex > turnIndex || (it.turnIndex === turnIndex && !kept.has(it.id)),
      );
    },

    /**
     * Test-only synchronous flush of every per-item streaming smoother
     * in this pane. Snaps each active smoother so items[].summary
     * reflects the full received text immediately, then disposes the
     * entry. Used by tests that assert summary content right after
     * applying deltas without waiting for the smoother's rAF schedule.
     * Not part of the production surface.
     */
    __flushItemSmoothersForTest(): void {
      streamingReveal.__flushForTest();
    },

    /**
     * Test-only count of live per-item streaming smoothers. Lets dispose-
     * contract regressions assert directly on the map size for kinds with
     * no other observable (assistant_text has no live-tail accessor). Not
     * part of the production surface.
     */
    __itemSmootherCountForTest(): number {
      return streamingReveal.__smootherCountForTest();
    },

    /**
     * Test-only: is the "ids the wire touched while a window sync was in
     * flight" ledger still armed? Non-null outside a load leg means a
     * clear/throw path leaked it, which silently changes how the NEXT
     * page application classifies pre-existing rows. Not part of the
     * production surface.
     */
    __syncLedgerArmedForTest(): boolean {
      return switchLoad.getLiveTouchedDuringSync() !== null;
    },

    /** Append a streaming text delta to a loaded row. See threadItemStreamApply.ts. */
    applyItemDelta(evt: ItemDeltaEvent): void {
      itemStream.applyItemDelta(evt);
    },

    /** Replace a loaded row's re-validated meta blob. See threadItemStreamApply.ts. */
    applyItemMeta(evt: ItemMetaEvent): void {
      itemStream.applyItemMeta(evt);
    },

    /** Apply a field patch to a loaded row. See threadItemStreamApply.ts. */
    applyItemPatch(evt: ItemPatchEvent): void {
      itemStream.applyItemPatch(evt);
    },

    // ---- Per-row UI state (survives windowing remount) ----
    expansionStateFor: rowUiState.expansionStateFor,
    retainExpansionStateFor: rowUiState.retainExpansionStateFor,
    expansionStateForPayload: rowUiState.expansionStateForPayload,
    retainExpansionStateForPayload: rowUiState.retainExpansionStateForPayload,
    isSubagentGroupExpanded: rowUiState.isSubagentGroupExpanded,
    /**
     * Expansion toggle with live eviction on collapse: the settled rows
     * of a card the user just closed fold out of pane memory (counts and
     * preview survive via the fold registry; the rows re-hydrate from
     * SQLite on the next expand). Active rows stay — the delta pipeline
     * requires streaming rows to exist in the window.
     */
    toggleSubagentGroupExpanded(groupKey: string): boolean {
      const willExpand = rowUiState.toggleSubagentGroupExpanded(groupKey);
      if (!willExpand) subagentMemory.evictCollapsedSubtree(groupKey);
      return willExpand;
    },
    /** Live fold aggregate for a launch anchor — MessageTimeline threads
     *  this into the grouping pipeline. Reads are revision-driven: every
     *  fold mutation rides a timelineRevision bump. */
    subagentLiveAggregate(anchorId: string): SubagentFoldAggregate | undefined {
      return subagentMemory.aggregate(anchorId);
    },
    /** Clamped user-message text the reader opened. Ephemeral per session —
     *  nothing about it goes to the backend. */
    isUserMessageExpanded: rowUiState.isUserMessageExpanded,
    setUserMessageExpanded: rowUiState.setUserMessageExpanded,
    diffCardExpandedOverride: rowUiState.diffCardExpandedOverride,
    setDiffCardExpanded: rowUiState.setDiffCardExpanded,
    /** Validity stamp for replaying a measured-size priors snapshot across a
     *  thread switch — see utils/virtual/priors.ts. */
    expansionSignature: rowUiState.expansionSignature,
    hasUserExpansionWithin: rowUiState.hasUserExpansionWithin,
    activityRuns,
    attachmentCacheFor: rowUiState.attachmentCacheFor,
    // Retention arrives from the CHAT MessageTimeline's prune pass and
    // describes only that instance's revealed rows — but the row-UI
    // store is shared with the agent companion's scoped timeline (whose
    // own prune is deliberately a no-op, agentScopeView.svelte.ts). An
    // open agent pane's rows must survive this pass or its attachment
    // blobs, expansion handles, and thinking tails get disposed out
    // from under a mounted surface, so retention is widened with the
    // scope trail's whole subtree before anything is dropped.
    pruneRowUiState(retention: RowUiStateRetention): void {
      const widened = widenRetentionForAgentPane(retention);
      rowUiState.pruneRowUiState(widened);
      streamingReveal.pruneSettledThinkingTails(widened.itemIds);
    },
    // Full revealed text for a reasoning-tail row. Live while the row
    // streams and retained across a content-consistent settle (see
    // threadRevealSmoothers.ts `itemLiveThinkingTail` for the lifetime
    // and the wrap-stability rationale). Returns null once the entry is
    // dropped (overwrite settle, removal, offscreen prune, thread
    // switch) — callers fall back to `item.summary`.
    liveThinkingTailForItem(itemId: string): string | null {
      return streamingReveal.liveThinkingTailFor(itemId);
    },

    /**
     * True while a per-item smoother still owns this row's summary
     * writes — i.e. the reveal is mid-drain, including the multi-second
     * tail AFTER the wire settles the item's status to terminal.
     * Reactive (SvelteMap-backed): row components derive their rendered
     * streaming mode from `status === 'streaming' || isItemSmoothing`,
     * so the streaming markdown guards hold until the drain finishes
     * rather than dropping at wire settle while text is still growing.
     */
    isItemSmoothing(itemId: string): boolean {
      return streamingReveal.isSmoothing(itemId);
    },

    get assistantRevealRegistrationGeneration() {
      return streamingReveal.assistantRevealRegistrationGeneration;
    },
    registerAssistantRevealSink: streamingReveal.registerAssistantRevealSink,
    assistantMarkdownParserSource(
      itemId: string,
      canonicalSource: string,
      renderContext: StreamingAssistantRenderContext,
    ): string {
      return streamingReveal.assistantParserSource(itemId, canonicalSource, renderContext);
    },
    assistantMarkdownSourceAppend(itemId: string, parserSource: string) {
      return streamingReveal.assistantSourceAppend(itemId, parserSource);
    },

    // Snap every behind smoother straight to its full received text on
    // visibilitychange → visible or completed reconnect recovery. See threadStreamingReveal.svelte.ts
    // `snapAllToReceived` for the full rationale.
    snapSmoothersToReceived(): void {
      streamingReveal.snapAllToReceived();
    },

    /**
     * Reveal gate for the timeline. While a turn streams, this is the
     * (turnIndex, itemIndex) of the top-level item currently revealing;
     * MessageTimeline withholds nodes after it via `sliceRevealedNodes` so
     * the next row waits for the current item's reveal to drain. `null`
     * outside live streaming — render everything. See
     * threadRevealGate.svelte.ts `recomputeReveal`.
     */
    get revealBoundary(): RevealBoundary | null {
      return streamingReveal.revealBoundary;
    },

    /**
     * How many rows the reveal queue is still draining in this pane.
     * Mirrors `isItemSmoothing` one level up: that answers the question per
     * row, this answers it for the pane, in one map-size read.
     *
     * It exists for MEASUREMENT and nothing else — the harness's
     * reveal-drain probe (utils/revealDrainProbe.ts) polls it so a bench or
     * a profile window can end when the user-visible stream has actually
     * finished rather than when the wire did. Nothing here may be used to
     * skip, rush or pop the drain.
     */
    get smoothingItemCount(): number {
      return streamingReveal.smootherCount();
    },

    /**
     * The pane's error-writing chokepoint. Every writer below is a thin
     * wrapper over this pair; see threadPaneErrors.svelte.ts for the
     * kinds and the resolution rule.
     */
    setPaneError: paneErrors.set,
    clearPaneError: paneErrors.clear,

    /**
     * Untagged write — the grab-bag slot (rename failed, git action
     * failed, workspace prep failed, …). It never touches another kind's
     * row: a live `history-load` banner keeps its Retry and this message
     * renders as its own row beneath it. `null` keeps its historical
     * meaning of "clear every banner".
     */
    setGeneralError(message: string | null): void {
      if (message === null) paneErrors.clear();
      else paneErrors.set(message, 'general');
    },

    setSessionError(message: string): void {
      paneErrors.set(message, 'session');
    },

    setHistoryLoadError(message: string | null): void {
      if (message === null) paneErrors.clear('history-load');
      else paneErrors.set(message, 'history-load');
    },

    /** The banner's Dismiss button: clears the surface, whatever is on it. */
    clearGeneralError(): void {
      paneErrors.clear();
    },

    /**
     * Clears the session-death message only. Called from the
     * `provider:turn_started` handler so a fresh turn auto-dismisses the
     * stale "session died" banner without clobbering orthogonal errors
     * (rename failed, git status, thread load).
     */
    clearSessionError(): void {
      paneErrors.clear('session');
    },

    setSendInFlight(value: boolean): void {
      sendInFlight = value;
    },

    /**
     * Open the one-shot structural-append spring window for a mutation
     * this pane is about to apply outside the wire-upsert path. Sole
     * external caller today: the composer's optimistic user-send, so the
     * just-sent message glides in through the spring instead of
     * sync-pinning (the send deliberately does NOT stamp the
     * live-content latch — one append wants one spring window, not
     * 500ms of spring eligibility for unrelated reflows). Owns the
     * loading/discussion gates; call it synchronously BEFORE the item
     * mutation so the window is open when the flush delivers geometry.
     */
    armStructuralSpring,

    trackOptimisticItem(id: string): void {
      optimisticItemIds.add(id);
    },

    isOptimisticItem(id: string): boolean {
      return optimisticItemIds.has(id);
    },

    untrackOptimisticItem(id: string): void {
      optimisticItemIds.delete(id);
    },

    setContextWindow(data: ContextWindow): void {
      contextWindow = normalizeContextWindowForThread(data, thread);
    },

    clearContextWindow(): void {
      contextWindow = null;
    },

    setProviderBanner(status: ProviderStatusEvent | null | undefined): void {
      providerBanner = status;
    },

    setProviderSessionAccount(account: ProviderSessionAccountEvent | null): void {
      updateProviderSessionAccount(account);
    },

    // --- Turn lifecycle mutations (threadPaneTurns.svelte.ts) ---

    setActiveTurn: paneTurns.setActiveTurn,
    settleTurn: paneTurns.settleTurn,
    clearActiveTurn: paneTurns.clearActiveTurn,
    clearTurnState: paneTurns.clearTurnState,

    // --- Live todo (activity rail Todos segment) ---

    get liveTodo() {
      return liveTodoState.liveTodo;
    },
    get liveTodoShowAll() {
      return liveTodoState.liveTodoShowAll;
    },

    /**
     * Replace the live-todo snapshot. Called from the
     * `provider:todo_update` listener for both Claude TodoWrite and
     * Codex update_plan. Empty step arrays clear the panel rather than
     * render an empty state. When every step is `completed`, schedule
     * the auto-hide timer; any subsequent update cancels the pending
     * timer so a late "now there's a new step" snapshot revives the
     * panel cleanly.
     *
     * Open/show-all state is intentionally NOT reset here — those are
     * per-thread user preferences (liveTodoUiPrefs) that should survive
     * the todo list briefly disappearing and reappearing within a thread.
     */
    setLiveTodo(steps: TodoStep[]): void {
      // The provider:todo_update listener (eventsProvider.ts:
      // applyTodoUpdate) is the wire boundary and validates `steps` is
      // an array before calling here; trust the input from that point on.
      // Subtract steps that the previous all-completed cycle already
      // cleared so the agent's full-list re-emission doesn't repaint
      // those rows under a new logical todo cycle.
      liveTodoState.setLiveTodo(steps);
    },

    /**
     * Drop the live-todo snapshot without waiting for the auto-hide
     * timer. Per-thread UI prefs are NOT cleared — the user's "I had
     * this open" preference persists across todo-clear and across
     * thread switches within the same session.
     *
     * Explicit clear also resets the "cleared cycle" set: the user's
     * mental model is "no todos, fresh start", and any subsequent
     * snapshot should be shown verbatim rather than filtered against
     * a prior auto-hide cycle.
     */
    clearLiveTodo(): void {
      liveTodoState.clearLiveTodo();
    },

    /** Toggle the "Show X more…" reveal under the truncated list. */
    toggleLiveTodoShowAll(): void {
      liveTodoState.toggleLiveTodoShowAll(thread?.id ?? null);
    },

    // --- Activity rail (consolidated working/todos/background) ---

    get activityRailTodosOpen() {
      return liveTodoState.activityRailTodosOpen;
    },
    get activityRailBackgroundOpen() {
      return liveTodoState.activityRailBackgroundOpen;
    },
    get activityRailInputCollapsed() {
      return liveTodoState.activityRailInputCollapsed;
    },

    /** Toggle the Todos accordion body inside the activity rail. */
    toggleActivityRailTodos(): void {
      liveTodoState.toggleActivityRailTodos(thread?.id ?? null);
    },

    /** Toggle the Background accordion body inside the activity rail. */
    toggleActivityRailBackground(): void {
      liveTodoState.toggleActivityRailBackground(thread?.id ?? null);
    },

    /**
     * Collapse/expand the pending-user-input popup from the activity-rail
     * chip. Per-thread sticky like the todos/background toggles: the state
     * survives thread switches and is inherited by the next input request
     * in the same thread (the chip stays visible while collapsed, so an
     * inherited-collapsed request is always one click from expanded).
     */
    toggleActivityRailInputCollapsed(): void {
      liveTodoState.toggleActivityRailInputCollapsed(thread?.id ?? null);
    },

    replaceThread(nextThread: Thread): void {
      if (
        thread &&
        (thread.provider !== nextThread.provider || thread.model !== nextThread.model)
      ) {
        updateEffectiveModel('');
      }
      assignThread(nextThread);
      contextWindow = seedContextWindow(nextThread);
    },

    setEffectiveModel(model: string): void {
      updateEffectiveModel(model);
    },

    applyEffectiveModel(model: string, revision: number): void {
      if (!Number.isSafeInteger(revision) || revision < effectiveModelBackendRevision) return;
      effectiveModelBackendRevision = revision;
      updateEffectiveModel(model);
    },

    setShowTerminal(value: boolean): void {
      // Bottom drawer mount/unmount reflows the chat column. Hold a brief
      // lease on a real visibility change so the controller's content-RO
      // sync-pin no-ops while the column's clientHeight is settling.
      if (value !== showTerminal) leaseDuringSettle(paneScroll.controller);
      showTerminal = value;
      // Scope the focus intent to the current open session: if the drawer is
      // hidden before it ever mounted to consume the request (e.g. a rapid
      // open→close), drop it so a later visibility-only reopen — or a
      // thread-restore that mounts the drawer with showTerminal persisted —
      // doesn't inherit a stale "steal focus" intent.
      if (!value) pendingTerminalFocus = false;
    },

    /**
     * Latch intent to move DOM focus into the terminal once its drawer mounts.
     * Called by runTerminalToggle BEFORE setShowTerminal(true) so the flag is
     * already set when the (lazily-loaded) drawer's onMount consumes it,
     * however many frames later the import resolves.
     */
    requestTerminalFocus(): void {
      pendingTerminalFocus = true;
    },

    /**
     * Read-and-clear the terminal focus intent. Returns true at most once per
     * requestTerminalFocus() so a drawer remount (e.g. {#key threadId}) can't
     * re-grab focus the user didn't ask for.
     */
    consumeTerminalFocusRequest(): boolean {
      const requested = pendingTerminalFocus;
      pendingTerminalFocus = false;
      return requested;
    },

    togglePlanSidebar: companions.togglePlanSidebar,
    setShowPlanSidebar: companions.setShowPlanSidebar,
    toggleReviewPane: companions.toggleReviewPane,
    setShowReviewPane: companions.setShowReviewPane,
    openAgentPane: companions.openAgentPane,
    closeAgentPane: companions.closeAgentPane,

    /** Single-message merge for a live `discussion:message` push, or the
     * message `PostChannelMessage` itself returns on a successful post. */
    applyChannelMessage(message: ChannelMessage): void {
      channelState.applyMessage(message);
    },

    /** Bulk merge for an initial channel load or gap-recovery resync
     * page — see `eventsDiscussion.ts`'s `refreshDiscussionChannel`. */
    applyChannelMessages(messages: ChannelMessage[]): void {
      channelState.applyMessageBatch(messages);
    },

    /** Full deliberation-FSM snapshot apply, shared by the initial load
     * and every `discussion:state` push. */
    applyChannelState(payload: ChannelStatePayload): void {
      channelState.applyState(payload);
    },

    clearChannel(): void {
      channelState.clear();
    },

  };
}

export type ThreadPane = ReturnType<typeof createThreadPane>;
