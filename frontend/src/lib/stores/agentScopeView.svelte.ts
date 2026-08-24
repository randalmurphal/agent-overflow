// Scoped ThreadPane facade for the agent companion pane.
//
// The agent pane is a NORMAL thread pane — the real MessageTimeline with
// its virtualizer, scroll physics, activity runs, paging plumbing — whose
// visible window is one subagent launch's subtree. Rather than teach the
// timeline a "scope mode", this module answers the ThreadPane surface
// with a Proxy over the source pane that overrides exactly the members
// where a scoped view legitimately differs, and forwards everything else
// (item resolution, payload loads, approvals, expansion registries,
// live-aggregate reads) so nothing here can drift from the chat surface.
//
// The override table IS the design — each entry names why the scoped
// view diverges:
//
// - `paneId` / `scrollStateKey`: distinct identity. chatDomIds scopes
//   disclosure ids by paneId (the same item can be mounted in both
//   surfaces at once), and timelineRestore keys scroll snapshots and
//   restore bookkeeping by scrollStateKey (per SCOPE, so an agent pane's
//   position never clobbers the main timeline's saved position).
// - `items`: the scope's loaded subtree. Direct children get their
//   `parentId` LIFTED (cleared) so the grouping treats them as this
//   surface's top level. A direct child launch remains a card, but its own
//   transcript stays outside this scope. Opening that row changes scope
//   through the breadcrumb instead of recursively embedding panes. Read
//   groups still group and activity runs still wrap. Completion siblings of
//   direct child launches ride along by `completionOf` (they carry no
//   parentId of their own), so nested cards fold status correctly.
// - `revealBoundary`: null. The reveal gate sequences TOP-LEVEL rows of
//   the main transcript; child rows were never reveal-sequenced, and a
//   boundary id from the main thread must not withhold scoped rows.
// - `timelineTurns`: the scope IS one turn. A subagent's rows are written
//   at the main thread's write head across however many provider turns
//   it outlives, so keying the response decorations on `item.turnIndex`
//   plus the thread's active/settled turn put a "Response 1m 58s" pill
//   on a still-running agent the moment the main turn settled (live
//   regression 2026-08-22). Here every scoped row shares one key; the
//   turn is active while the scoped launch runs and settles on the
//   launch's own completion, with the agent's own duration.
// - `activityRuns`: an own registry. Run membership differs per surface
//   (the scoped list has different top-level rows), and collapse state
//   is a view concern, so sharing the source registry would let one
//   surface's collapse mutate the other's geometry.
// - scroll controller / scroll-to-item: own slots, same reason — the
//   source pane's slots belong to the main MessageTimeline instance.
// - paging (`loadOlder`/`loadNewer`/`loadUntilItem`, the `hasMore*` and
//   `loading*` flags): a scope is not a turn window. Everything the
//   scope can show is either loaded or fetched wholesale through
//   `ensureSubagentChildren` (the pane body drives that; see
//   AgentPane.svelte), so the timeline's edge-paging must never fire.
// - `openAgentPane`: descend-in-place. Inside the pane, opening a child
//   card grows the breadcrumb (`pushScope`) instead of re-seeding the
//   companion from the outside.
// - `pruneRowUiState`: no-op. Row-UI state (expansion handles, attachment
//   blob caches, thinking tails) is SHARED with the source pane by
//   design, and each MessageTimeline instance's prune pass computes
//   retention from ITS OWN revealed rows — a scoped instance's retention
//   describes one subtree, so letting it reach the shared store would
//   revoke the attachment blobs and expansion state of every main-
//   timeline row (live incident 2026-08-22: pasted screenshots went
//   dead the moment the agent pane's prune ran). The host pane's own
//   prune stays the one bounded-memory owner, and it spares the open
//   scope's rows in return (thread.svelte.ts widens its retention via
//   `collectAgentScopeRetainedIds`).
//
// One instance per (pane body mount × scope): AgentPane keys the
// timeline on the scope id, so a scope swap builds a fresh view and a
// fresh MessageTimeline — scroll restore, warmup, and run identity all
// start clean, exactly like a thread switch.

import type { ThreadPane } from './thread.svelte';
import type { AgentPaneState } from './agentPane.svelte';
import type { Item } from '../types/models';
import { createThreadActivityRuns } from './threadActivityRuns.svelte';
import {
  activityRunDefaultCollapsed,
  activityRunWindowRows,
} from './activityRunPrefs.svelte';
import type {
  LoadOlderResult,
  PaneScrollController,
  ScrollToItemRequest,
} from './threadPaneShared';
import type { TimelineTurnFacet } from './threadTurnProjection';

/** The one turn key every scoped row shares (see `timelineTurns` above). */
const AGENT_SCOPE_TURN_KEY = 0;

export interface AgentScopeView {
  /** The ThreadPane facade MessageTimeline mounts. */
  readonly pane: ThreadPane;
  /** The scope's loaded subtree (what `pane.items` answers). */
  readonly items: Item[];
  /** Release the view's own registries. Call on unmount. */
  dispose(): void;
}

/** Settled no-op paging result: a scope window has no edges to page. */
const NO_PAGE: Promise<LoadOlderResult> = Promise.resolve({
  insertedBeforeWindow: false,
  insertedRows: false,
  status: 'noop',
});

/**
 * Every loaded row the scope at `scopeItemId` renders or needs as a direct
 * navigation edge: the scope row itself, its direct children, and the
 * completion siblings of those rows. A child's descendants belong to the
 * child's own pane scope and are deliberately not retained here.
 *
 * Two consumers, one truth: the facade's item window filters through
 * this set, and the source pane's row-UI prune widens its retention
 * with it so the chat timeline's bounded-memory pass cannot dispose
 * state under rows the agent pane has mounted.
 */
export function collectAgentScopeRetainedIds(
  items: readonly Item[],
  scopeItemId: string,
): Set<string> {
  const retained = new Set<string>();
  if (!scopeItemId) return retained;
  for (const item of items) {
    if (item.parentId === scopeItemId) retained.add(item.id);
  }
  retained.add(scopeItemId);
  for (const item of items) {
    if (item.completionOf && retained.has(item.completionOf)) {
      retained.add(item.id);
    }
  }
  return retained;
}

export function createAgentScopeView(
  sourcePane: ThreadPane,
  agent: AgentPaneState,
  scopeItemId: string,
): AgentScopeView {
  // ---- Scoped item window ---------------------------------------------
  // Recomputed per source timelineRevision (the projection reads items
  // untracked behind that revision, so identity churn outside a revision
  // bump would be invisible anyway — matching the source pane's own
  // contract). Direct children are cloned with parentId lifted. A nested
  // launch's descendants do not enter this window.
  //
  // One ordered pass over the source items, so the window keeps the
  // timeline's document order exactly. The membership set comes from
  // `collectAgentScopeRetainedIds` (direct rows + completion siblings); the
  // SCOPE's own row and its completion sibling stay out — they feed the
  // pane's breadcrumb and status line, not the transcript.
  let scopedItems = $derived.by<Item[]>(() => {
    void sourcePane.timelineRevision;
    if (!scopeItemId) return [];
    const retained = collectAgentScopeRetainedIds(sourcePane.items, scopeItemId);
    const out: Item[] = [];
    for (const item of sourcePane.items) {
      if (item.id === scopeItemId || !retained.has(item.id)) continue;
      if (item.completionOf === scopeItemId) continue;
      out.push(item.parentId === scopeItemId ? { ...item, parentId: undefined } : item);
    }
    return out;
  });

  // ---- Scope lifecycle as the timeline's turn ---------------------------
  // Status reads go through the SOURCE pane's live row (`getItemById`,
  // the row's own box), so a launch flipping to terminal re-derives
  // without a structural revision. The completion sibling is the status
  // source once it exists — same rule the card and the composer shell
  // follow. Its MEMBERSHIP comes from the array (structure); its fields
  // must not, because a patch to the row is written in place and the
  // array signal stays silent for it.
  let scopeLaunch = $derived.by<Item | undefined>(() => {
    void sourcePane.timelineRevision;
    return scopeItemId ? sourcePane.getItemById(scopeItemId) : undefined;
  });
  let scopeCompletion = $derived.by<Item | undefined>(() => {
    void sourcePane.timelineRevision;
    if (!scopeItemId) return undefined;
    const completion = sourcePane.items.find((item) => item.completionOf === scopeItemId);
    return completion ? (sourcePane.getItemById(completion.id) ?? completion) : undefined;
  });
  const timelineTurns: TimelineTurnFacet = {
    keyOf: () => AGENT_SCOPE_TURN_KEY,
    get activeKey() {
      const status = (scopeCompletion ?? scopeLaunch)?.status;
      return status === 'running' || status === 'streaming' ? AGENT_SCOPE_TURN_KEY : null;
    },
    get settled() {
      const launch = scopeLaunch;
      const statusItem = scopeCompletion ?? launch;
      if (!launch || !statusItem) return null;
      if (statusItem.status === 'running' || statusItem.status === 'streaming') return null;
      return {
        key: AGENT_SCOPE_TURN_KEY,
        startedAt: launch.createdAt,
        completedAt: statusItem.updatedAt,
      };
    },
  };

  // ---- Own view registries ---------------------------------------------
  let scrollController: PaneScrollController | null = $state.raw(null);
  let scrollToItemRequest = $state.raw<ScrollToItemRequest>({ itemId: '', nonce: 0 });
  const activityRuns = createThreadActivityRuns({
    defaultCollapsed: () => activityRunDefaultCollapsed(),
    windowRows: () => activityRunWindowRows(),
    scrollController: () => scrollController,
  });

  const overrides: Record<PropertyKey, unknown> = {
    get paneId() {
      return `${sourcePane.paneId}~agent`;
    },
    get scrollStateKey() {
      return `${sourcePane.threadId ?? ''}~agent:${scopeItemId}`;
    },
    get agentScopeRootId() {
      return scopeItemId;
    },
    get items() {
      return scopedItems;
    },
    get revealBoundary() {
      return null;
    },
    get timelineTurns() {
      return timelineTurns;
    },
    get activityRuns() {
      return activityRuns;
    },
    get scrollController() {
      return scrollController;
    },
    attachScrollController(controller: PaneScrollController): void {
      scrollController = controller;
    },
    detachScrollController(controller: PaneScrollController): void {
      if (scrollController === controller) scrollController = null;
    },
    get scrollToItemRequest() {
      return scrollToItemRequest;
    },
    requestScrollToItem(itemID: string): void {
      if (!itemID) return;
      scrollToItemRequest = { itemId: itemID, nonce: scrollToItemRequest.nonce + 1 };
    },
    // A scope never edge-pages; its rows arrive wholesale via
    // ensureSubagentChildren (driven by the pane body).
    loadOlder: () => NO_PAGE,
    loadNewer: () => NO_PAGE,
    loadUntilItem: (itemID: string) =>
      Promise.resolve(scopedItems.some((item) => item.id === itemID)),
    get hasMoreHistory() {
      return false;
    },
    get hasMoreNewer() {
      return false;
    },
    get loadingOlder() {
      return false;
    },
    get loadingNewer() {
      return false;
    },
    get hasDeferredRecentWindowPrune() {
      return false;
    },
    retryDeferredRecentWindowPrune(): void {},
    // See the module header: a scoped instance's retention describes one
    // subtree, so its prune pass must never reach the SHARED row-UI
    // store. The host pane's own prune is the one bounded-memory owner.
    pruneRowUiState(): void {},
    // The scope's rows are already local (or arriving via hydration);
    // the thread-level loading states describe the SOURCE window.
    get loading() {
      return false;
    },
    get showLoadingSpinner() {
      return false;
    },
    openAgentPane(launchItemId: string, label: string): void {
      agent.pushScope(launchItemId, label);
    },
  };

  const pane = new Proxy(sourcePane, {
    get(target, prop) {
      if (prop in overrides) {
        return Reflect.get(overrides, prop);
      }
      return Reflect.get(target, prop, target);
    },
  }) as ThreadPane;

  return {
    pane,
    get items() {
      return scopedItems;
    },
    dispose() {
      activityRuns.clear();
    },
  };
}
