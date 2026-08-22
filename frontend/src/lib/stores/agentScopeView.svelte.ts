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
//   surface's top level — nested launches still become cards, read
//   groups still group, activity runs still wrap. Completion siblings of
//   subtree launches ride along by `completionOf` (they carry no
//   parentId of their own), so nested cards fold status correctly.
// - `revealBoundary`: null. The reveal gate sequences TOP-LEVEL rows of
//   the main transcript; child rows were never reveal-sequenced, and a
//   boundary id from the main thread must not withhold scoped rows.
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

export function createAgentScopeView(
  sourcePane: ThreadPane,
  agent: AgentPaneState,
  scopeItemId: string,
): AgentScopeView {
  // ---- Scoped item window ---------------------------------------------
  // Recomputed per source timelineRevision (the projection reads items
  // untracked behind that revision, so identity churn outside a revision
  // bump would be invisible anyway — matching the source pane's own
  // contract). Direct children are cloned with parentId lifted; deeper
  // rows keep their real parent chain.
  let scopedItems = $derived.by<Item[]>(() => {
    void sourcePane.timelineRevision;
    if (!scopeItemId) return [];
    const byParent = new Map<string, Item[]>();
    for (const item of sourcePane.items) {
      const pid = item.parentId;
      if (!pid) continue;
      let bucket = byParent.get(pid);
      if (!bucket) byParent.set(pid, (bucket = []));
      bucket.push(item);
    }
    const out: Item[] = [];
    const subtreeIds = new Set<string>([scopeItemId]);
    const stack = [scopeItemId];
    while (stack.length > 0) {
      const kids = byParent.get(stack.pop()!);
      if (!kids) continue;
      for (const kid of kids) {
        subtreeIds.add(kid.id);
        out.push(kid.parentId === scopeItemId ? { ...kid, parentId: undefined } : kid);
        stack.push(kid.id);
      }
    }
    // Completion siblings carry `completionOf`, not a parentId, so the
    // parent walk above cannot reach them. A nested launch's completion
    // must ride along for its card to fold status; the SCOPE's own
    // completion stays out (it feeds the pane's status line instead).
    for (const item of sourcePane.items) {
      if (!item.completionOf || item.completionOf === scopeItemId) continue;
      if (subtreeIds.has(item.completionOf) && !subtreeIds.has(item.id)) {
        out.push(item);
      }
    }
    return out;
  });

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
    get items() {
      return scopedItems;
    },
    get revealBoundary() {
      return null;
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
