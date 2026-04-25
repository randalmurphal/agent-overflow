<script lang="ts" module>
  type ScrollSnapshot =
    | { kind: 'bottom' }
    | { kind: 'anchor'; itemId: string; offsetTop: number };

  const threadScrollSnapshots = new Map<string, ScrollSnapshot>();
  const MAX_THREAD_SCROLL_SNAPSHOTS = 100;

  function setThreadScrollSnapshot(threadId: string, snapshot: ScrollSnapshot): void {
    if (threadScrollSnapshots.has(threadId)) {
      threadScrollSnapshots.delete(threadId);
    }
    threadScrollSnapshots.set(threadId, snapshot);
    while (threadScrollSnapshots.size > MAX_THREAD_SCROLL_SNAPSHOTS) {
      const oldestThreadId = threadScrollSnapshots.keys().next().value;
      if (oldestThreadId === undefined) break;
      threadScrollSnapshots.delete(oldestThreadId);
    }
  }

  export function clearMessageTimelineScrollSnapshotsForTest(): void {
    threadScrollSnapshots.clear();
  }
</script>

<script lang="ts">
  import { onDestroy, tick, untrack } from 'svelte';
  import type { SettledTurn, ThreadPane } from '../../stores/thread.svelte';
  import type { Item } from '../../types/models';
  import { addToast } from '../../stores/toast.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { isScrollPinnedToBottom } from '../../utils/scrollPosition';
  import { groupItemsBySubagent, type TimelineNode } from '../../utils/subagentGrouping';
  import {
    buildVirtualLayout,
    computeVirtualWindow,
    timelineNodeKey,
  } from '../../utils/timelineVirtualization';
  import {
    DEFAULT_TIMELINE_ROW_HEIGHT,
    estimateTimelineNodeHeight,
    isLastRootInTurn as nodeIsLastRootInTurn,
    rootTurnIndex,
    timelineNodeHeightSignature,
  } from '../../utils/timelineRowHeights';
  import Button from '../primitives/Button.svelte';
  import { turnSummaryIsMeaningful, type TurnDiffView } from '../../utils/turnDiffSummary';
  import ChangedFilesTree from './ChangedFilesTree.svelte';
  import ChatWorkingIndicator from './ChatWorkingIndicator.svelte';
  import CompletionDivider from './CompletionDivider.svelte';
  import SubagentGroup from './SubagentGroup.svelte';
  import { createTimelineMeasurementActions } from './timelineMeasurementActions';
  import TimelineLeaf from './TimelineLeaf.svelte';
  import TurnDiffBadge from './TurnDiffBadge.svelte';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import {
    isUiRenderTraceEnabled,
    recordUiTrace,
    scheduleDomUiTrace,
    summarizeItemsForTrace,
  } from '../../utils/uiRenderTrace';

  let {
    pane,
    onImageExpand,
  }: {
    pane: ThreadPane;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
  } = $props();

  /**
   * Decide whether the completion divider renders immediately before this
   * node in the timeline flow. Pure, exported for unit-testing via the
   * MessageTimeline test — checks:
   *   - there's a settled turn to render for
   *   - that turn reported a terminal assistant message id
   *   - the current node is the leaf whose id matches that message
   * Subagent-group nodes and non-matching leaves render nothing.
   *
   * Spec: docs/architecture/turn-lifecycle.md §UI components driven by
   * this state. See also t3-code's deriveCompletionDividerBeforeEntryId
   * (apps/web/src/session-logic.ts) — we skip the time-range fallback
   * since agent-overflow persists assistantMessageId directly on the
   * turns row.
   */
  export function shouldRenderDividerBefore(
    node: TimelineNode,
    turn: SettledTurn | null,
  ): boolean {
    if (!turn) return false;
    if (!turn.assistantMessageId) return false;
    if (node.kind !== 'leaf') return false;
    return node.item.id === turn.assistantMessageId;
  }

  let scrollContainer: HTMLDivElement | undefined = $state(undefined);
  let viewportScrollTop = $state(0);
  let viewportHeight = $state(0);
  let rowHeightRevision = $state(0);
  const rowHeights = new Map<string, number>();
  const rowHeightSignatures = new Map<string, string>();
  const OVERSCAN_PX = 900;

  let userPinnedToBottom = $state(true);
  let restoredThreadId: string | null = $state(null);
  let scrollSnapshotThreadId: string | null = $state(null);
  let restoringScroll = false;

  function syncViewportState(): void {
    if (!scrollContainer) return;
    const { scrollTop, clientHeight } = scrollContainer;
    viewportScrollTop = scrollTop;
    viewportHeight = clientHeight;
  }

  function syncUserScrollState(): void {
    if (!scrollContainer) return;
    const { scrollTop, scrollHeight, clientHeight } = scrollContainer;
    syncViewportState();
    userPinnedToBottom = isScrollPinnedToBottom(scrollTop, scrollHeight, clientHeight);
  }

  function handleScroll(): void {
    liveAnchorRestoreToken += 1;
    initialScrollRestoreToken += 1;
    loadOlderRestoreToken += 1;
    pendingLiveAnchor = null;
    pendingLiveAnchorRevision = -1;
    if (initialRestoreFrame !== null) {
      cancelAnimationFrame(initialRestoreFrame);
      initialRestoreFrame = null;
      restoringScroll = false;
    }
    syncUserScrollState();
    saveScrollSnapshot();
    if (!userPinnedToBottom && bottomScrollFrame !== null) {
      cancelAnimationFrame(bottomScrollFrame);
      bottomScrollFrame = null;
    }
  }

  /**
   * Per-turn diff view lives on the pane and is incrementally maintained by
   * upsertItem. MessageTimeline consumes it read-only: a turn with an entry
   * renders the ChangedFilesTree; if the summary passes `isMeaningful` the
   * TurnDiffBadge renders too.
   */
  let turnDiffViews = $derived(pane.turnDiffViews);

  /**
   * Build the subagent-aware render tree. Items are grouped into subagent
   * cards when they declare a parentId matching a parent tool item;
   * otherwise they pass through as leaves. The function is pure and
   * deterministic, so `$derived` re-runs exactly when `pane.items` changes.
   */
  let groupedNodes = $derived<TimelineNode[]>(groupItemsBySubagent(pane.items));
  $effect.pre(() => {
    const showEndOfTurnDiffs = getSettings().showEndOfTurnDiffs;
    const latestSettledTurn = pane.latestSettledTurn;
    const diffViews = pane.turnDiffViews;
    const changed = invalidateChangedRowHeights(
      groupedNodes,
      showEndOfTurnDiffs,
      latestSettledTurn,
      diffViews,
    );
    if (changed) {
      rowHeightRevision += 1;
    }
  });
  let virtualLayout = $derived.by(() => {
    rowHeightRevision;
    return buildVirtualLayout(groupedNodes, rowHeights, estimateTimelineNodeHeight);
  });
  let virtualWindow = $derived(
    computeVirtualWindow(virtualLayout, viewportScrollTop, viewportHeight, OVERSCAN_PX),
  );

  $effect(() => {
    pane.threadId;
    pane.items.length;
    pane.timelineRevision;
    groupedNodes.length;

    if (!isUiRenderTraceEnabled()) return;
    recordUiTrace('timeline.state', {
      threadId: pane.threadId,
      itemCount: pane.items.length,
      timelineRevision: pane.timelineRevision,
      groupedNodeCount: groupedNodes.length,
      nodes: groupedNodes.slice(0, 120).map((node) => (
        node.kind === 'leaf'
          ? {
              kind: 'leaf',
              itemId: node.item.id,
              itemThreadId: node.item.threadId,
              itemKind: node.item.kind,
              turnIndex: node.item.turnIndex,
              orphan: node.orphan === true,
            }
          : {
              kind: 'group',
              parentId: node.parent.id,
              parentThreadId: node.parent.threadId,
              childCount: node.children.length,
              turnIndex: node.parent.turnIndex,
            }
      )),
      items: summarizeItemsForTrace(pane.items),
    });
    scheduleDomUiTrace('timeline', 'timeline.dom', () => ({
      threadId: pane.threadId,
      rowCount: scrollContainer?.querySelectorAll('[data-item-id]').length ?? 0,
      rows: Array.from(scrollContainer?.querySelectorAll<HTMLElement>('[data-item-id]') ?? [])
        .slice(0, 160)
        .map((el) => ({
          itemId: el.dataset.itemId ?? '',
          textPreview: (el.textContent ?? '').replace(/\s+/g, ' ').trim().slice(0, 120),
        })),
      scrollTop: scrollContainer ? Math.round(scrollContainer.scrollTop) : 0,
      scrollHeight: scrollContainer ? Math.round(scrollContainer.scrollHeight) : 0,
      clientHeight: scrollContainer ? Math.round(scrollContainer.clientHeight) : 0,
    }));
  });

  /**
   * Turn boundaries on the root-node stream: we emit the ChangedFilesTree
   * summary at the end of every turn that has diff activity. Subagent
   * children share their parent's turnIndex (triage uses LastTurnIndex for
   * child persistence), so the turn of a group node is the parent's turn.
   */
  function isLastRootInTurn(index: number): boolean {
    return nodeIsLastRootInTurn(index, groupedNodes);
  }

  function offsetForIndex(index: number): number {
    return virtualLayout.offsets[Math.min(Math.max(index, 0), virtualLayout.rows.length)] ?? 0;
  }

  function invalidateChangedRowHeights(
    nodes: TimelineNode[],
    showEndOfTurnDiffs: boolean,
    latestSettledTurn: SettledTurn | null,
    diffViews: ReadonlyMap<number, TurnDiffView>,
  ): boolean {
    const activeKeys = new Set<string>();
    let changed = false;
    for (let index = 0; index < nodes.length; index += 1) {
      const node = nodes[index];
      const key = timelineNodeKey(node);
      const signature = timelineNodeHeightSignature(
        node,
        index,
        nodes,
        showEndOfTurnDiffs,
        latestSettledTurn,
        diffViews,
      );
      activeKeys.add(key);
      if (rowHeightSignatures.get(key) === signature) continue;
      rowHeightSignatures.set(key, signature);
      if (rowHeights.delete(key)) changed = true;
    }
    for (const key of rowHeightSignatures.keys()) {
      if (!activeKeys.has(key)) {
        rowHeightSignatures.delete(key);
        if (rowHeights.delete(key)) changed = true;
      }
    }
    return changed;
  }

  const timelineMeasurementActions = createTimelineMeasurementActions({
    estimatedRowHeight: DEFAULT_TIMELINE_ROW_HEIGHT,
    getRowHeight: (key) => rowHeights.get(key),
    getScrollContainer: () => scrollContainer,
    getUserPinnedToBottom: () => userPinnedToBottom,
    onRowHeightChanged: () => {
      rowHeightRevision += 1;
    },
    setRowHeight: (key, height) => {
      rowHeights.set(key, height);
    },
    setScrollContainer: (node) => {
      scrollContainer = node;
    },
    syncViewportState,
  });
  const { measureScrollContainer, measureTimelineRow } = timelineMeasurementActions;

  function nodeContainsItem(node: TimelineNode, itemId: string): boolean {
    if (node.kind === 'leaf') {
      return node.item.id === itemId;
    }
    return node.parent.id === itemId || node.children.some((child) => nodeContainsItem(child, itemId));
  }

  /**
   * Suppress bottom stickiness while a Load Older round-trip is in
   * flight. Without this, a short thread whose whole window fits in the
   * viewport keeps `userPinnedToBottom` true throughout the prepend.
   * When items.length grows, the effect would scroll the container to
   * the new bottom a frame after our handleLoadOlder delta-apply —
   * snapping the user past the newly revealed history.
   */
  let suppressBottomAutoScroll = $state(false);
  let bottomAutoScrollSuppressionDepth = 0;
  let bottomScrollFrame: number | null = null;
  let initialRestoreFrame: number | null = null;
  let pendingLiveAnchor: Extract<ScrollSnapshot, { kind: 'anchor' }> | null = null;
  let pendingLiveAnchorRevision = -1;
  let observedLiveAnchorThreadId: string | null = null;
  let observedLiveAnchorRevision = -1;
  let liveAnchorRestoreToken = 0;
  let initialScrollRestoreToken = 0;
  let loadOlderRestoreToken = 0;

  function suppressBottomAutoScrollUntilReleased(): () => void {
    let released = false;
    bottomAutoScrollSuppressionDepth += 1;
    suppressBottomAutoScroll = true;
    return () => {
      if (released) return;
      released = true;
      bottomAutoScrollSuppressionDepth = Math.max(0, bottomAutoScrollSuppressionDepth - 1);
      suppressBottomAutoScroll = bottomAutoScrollSuppressionDepth > 0;
    };
  }

  onDestroy(() => {
    saveScrollSnapshot();
    if (initialRestoreFrame !== null) {
      cancelAnimationFrame(initialRestoreFrame);
      initialRestoreFrame = null;
    }
    if (bottomScrollFrame !== null) {
      cancelAnimationFrame(bottomScrollFrame);
      bottomScrollFrame = null;
    }
  });

  function snapshotThreadId(): string | null {
    return pane.threadId || null;
  }

  function saveScrollSnapshot(): void {
    const threadId = snapshotThreadId();
    if (!threadId) return;
    saveScrollSnapshotForThread(threadId, false);
  }

  function saveScrollSnapshotForThread(threadId: string, ignoreLoading: boolean): void {
    if (!scrollContainer || (!ignoreLoading && pane.loading) || restoredThreadId !== threadId) return;
    if (userPinnedToBottom) {
      setThreadScrollSnapshot(threadId, { kind: 'bottom' });
      return;
    }

    const anchor = firstVisibleItemAnchor();
    if (anchor) {
      setThreadScrollSnapshot(threadId, anchor);
    }
  }

  function firstVisibleItemAnchor(): ScrollSnapshot | null {
    if (!scrollContainer) return null;
    const viewport = scrollContainer.getBoundingClientRect();
    const itemElements = Array.from(
      scrollContainer.querySelectorAll<HTMLElement>('[data-item-id]'),
    );
    for (const el of itemElements) {
      const rect = el.getBoundingClientRect();
      if (rect.height <= 0) continue;
      if (rect.bottom < viewport.top) continue;
      const itemId = el.dataset.itemId ?? '';
      if (!itemId) continue;
      return {
        kind: 'anchor',
        itemId,
        offsetTop: rect.top - viewport.top,
      };
    }
    return null;
  }

  function scrollToBottom(): void {
    if (!scrollContainer) return;
    scrollContainer.scrollTop = Math.max(
      0,
      scrollContainer.scrollHeight - scrollContainer.clientHeight,
    );
    syncViewportState();
    userPinnedToBottom = true;
    saveScrollSnapshot();
  }

  function scheduleScrollToBottom(): void {
    if (!scrollContainer || bottomScrollFrame !== null) return;
    const targetThreadId = pane.threadId;
    bottomScrollFrame = requestAnimationFrame(() => {
      bottomScrollFrame = null;
      if (!scrollContainer || pane.threadId !== targetThreadId) return;
      if (!userPinnedToBottom) return;
      scrollToBottom();
    });
  }

  async function restoreInitialScroll(threadId: string, restoreToken: number): Promise<void> {
    if (!scrollContainer) return;
    const snapshot = threadScrollSnapshots.get(threadId);
    const releaseBottomAutoScroll = suppressBottomAutoScrollUntilReleased();
    const shouldContinue = () => (
      initialScrollRestoreToken === restoreToken
      && pane.threadId === threadId
      && restoredThreadId === threadId
      && scrollContainer !== undefined
    );
    try {
      if (snapshot?.kind === 'anchor') {
        const restored = await restoreAnchorSnapshot(snapshot, shouldContinue);
        if (restored) {
          syncUserScrollState();
          saveScrollSnapshot();
          return;
        }
      }
      if (!shouldContinue()) return;
      scrollToBottom();
    } finally {
      restoringScroll = false;
      releaseBottomAutoScroll();
    }
  }

  async function restoreAnchorSnapshot(
    snapshot: Extract<ScrollSnapshot, { kind: 'anchor' }>,
    shouldContinue: () => boolean = () => true,
  ): Promise<boolean> {
    if (!scrollContainer || !snapshot.itemId || !shouldContinue()) return false;
    const found = await pane.loadUntilItem(snapshot.itemId);
    if (!found || !scrollContainer || !shouldContinue()) return false;
    return restoreLoadedAnchorSnapshot(snapshot, shouldContinue);
  }

  async function restoreLoadedAnchorSnapshot(
    snapshot: Extract<ScrollSnapshot, { kind: 'anchor' }>,
    shouldContinue: () => boolean = () => true,
  ): Promise<boolean> {
    if (!scrollContainer || !snapshot.itemId) return false;
    await tick();
    if (!scrollContainer || !shouldContinue()) return false;

    const targetIndex = groupedNodes.findIndex((node) => nodeContainsItem(node, snapshot.itemId));
    if (targetIndex < 0) return false;

    const previousScrollTop = scrollContainer.scrollTop;
    scrollContainer.scrollTop = Math.max(0, offsetForIndex(targetIndex) - snapshot.offsetTop);
    const approximatedScrollTop = scrollContainer.scrollTop;
    syncViewportState();
    await tick();
    if (!scrollContainer || !shouldContinue()) {
      if (scrollContainer && scrollContainer.scrollTop === approximatedScrollTop) {
        scrollContainer.scrollTop = previousScrollTop;
        syncViewportState();
      }
      return false;
    }

    const el = scrollContainer.querySelector(`[data-item-id="${CSS.escape(snapshot.itemId)}"]`);
    if (!(el instanceof HTMLElement)) {
      if (scrollContainer.scrollTop === approximatedScrollTop) {
        scrollContainer.scrollTop = previousScrollTop;
        syncViewportState();
      }
      return false;
    }

    const viewport = scrollContainer.getBoundingClientRect();
    const rect = el.getBoundingClientRect();
    if (!shouldContinue()) {
      if (scrollContainer.scrollTop === approximatedScrollTop) {
        scrollContainer.scrollTop = previousScrollTop;
        syncViewportState();
      }
      return false;
    }
    scrollContainer.scrollTop += rect.top - viewport.top - snapshot.offsetTop;
    syncViewportState();
    return true;
  }

  $effect(() => {
    const threadId = pane.threadId;
    const loading = pane.loading;
    pane.items.length;
    rowHeightRevision;
    viewportHeight;
    const containerReady = scrollContainer !== undefined;

    if (!threadId || loading || !containerReady) return;
    if (restoredThreadId === threadId) return;
    restoredThreadId = threadId;
    restoringScroll = true;
    const restoreToken = ++initialScrollRestoreToken;
    initialRestoreFrame = requestAnimationFrame(() => {
      initialRestoreFrame = null;
      if (initialScrollRestoreToken !== restoreToken || pane.threadId !== threadId) {
        restoringScroll = false;
        return;
      }
      void restoreInitialScroll(threadId, restoreToken);
    });
  });

  $effect.pre(() => {
    const nextThreadId = pane.threadId;
    if (scrollSnapshotThreadId && scrollSnapshotThreadId !== nextThreadId) {
      saveScrollSnapshotForThread(scrollSnapshotThreadId, true);
      restoredThreadId = null;
      liveAnchorRestoreToken += 1;
      initialScrollRestoreToken += 1;
      loadOlderRestoreToken += 1;
      pendingLiveAnchor = null;
      pendingLiveAnchorRevision = -1;
    }
    scrollSnapshotThreadId = nextThreadId;
  });

  $effect.pre(() => {
    const revision = pane.timelineRevision;
    const threadId = pane.threadId;
    const containerReady = scrollContainer !== undefined;

    if (!threadId || !containerReady) return;
    if (observedLiveAnchorThreadId !== threadId) {
      liveAnchorRestoreToken += 1;
      observedLiveAnchorThreadId = threadId;
      observedLiveAnchorRevision = revision;
      pendingLiveAnchor = null;
      pendingLiveAnchorRevision = -1;
      return;
    }
    if (observedLiveAnchorRevision === revision) return;
    liveAnchorRestoreToken += 1;
    observedLiveAnchorRevision = revision;
    if (restoredThreadId !== threadId || restoringScroll) return;
    if (userPinnedToBottom) {
      pendingLiveAnchor = null;
      pendingLiveAnchorRevision = -1;
      return;
    }

    const anchor = firstVisibleItemAnchor();
    pendingLiveAnchor = anchor?.kind === 'anchor' ? anchor : null;
    pendingLiveAnchorRevision = pendingLiveAnchor ? revision : -1;
  });

  $effect(() => {
    const revision = pane.timelineRevision;
    const threadId = pane.threadId;
    const anchor = pendingLiveAnchor;
    const containerReady = scrollContainer !== undefined;

    if (!threadId || !containerReady || !anchor) return;
    if (pendingLiveAnchorRevision !== revision) return;
    if (restoredThreadId !== threadId || restoringScroll || userPinnedToBottom) return;

    pendingLiveAnchor = null;
    pendingLiveAnchorRevision = -1;
    const restoreThreadId = threadId;
    const restoreRevision = revision;
    const restoreToken = ++liveAnchorRestoreToken;
    const releaseBottomAutoScroll = suppressBottomAutoScrollUntilReleased();
    const shouldContinue = () => (
      liveAnchorRestoreToken === restoreToken
      && pane.threadId === restoreThreadId
      && pane.timelineRevision === restoreRevision
      && !userPinnedToBottom
      && !restoringScroll
    );
    void restoreLoadedAnchorSnapshot(anchor, shouldContinue).finally(() => {
      releaseBottomAutoScroll();
      if (!shouldContinue()) return;
      syncUserScrollState();
      saveScrollSnapshot();
    });
  });

  // Stick to bottom only when the user is already pinned to the bottom.
  $effect(() => {
    const threadId = pane.threadId;
    const loading = pane.loading;
    pane.items.length;
    pane.timelineRevision;
    pane.activeTurn?.turnId;
    rowHeightRevision;
    viewportHeight;
    const containerReady = scrollContainer !== undefined;

    if (!threadId || loading || !containerReady) return;
    if (restoredThreadId !== threadId || restoringScroll) return;
    if (suppressBottomAutoScroll) return;
    untrack(() => {
      if (userPinnedToBottom) scheduleScrollToBottom();
    });
  });

  /**
   * Fetch older history and preserve the user's visual anchor row. We
   * prefer restoring the actual first visible item because ancestor rows
   * can sit above the loaded floor; "inserted before items[0]" is not the
   * same thing as "inserted before what the user was reading".
   */
  async function handleLoadOlder(): Promise<void> {
    if (!scrollContainer) return;
    const prevScrollHeight = scrollContainer.scrollHeight;
    const prevScrollTop = scrollContainer.scrollTop;
    const anchor = firstVisibleItemAnchor();
    const restoreThreadId = pane.threadId;
    const restoreToken = ++loadOlderRestoreToken;
    const releaseBottomAutoScroll = suppressBottomAutoScrollUntilReleased();
    const shouldContinue = () => (
      loadOlderRestoreToken === restoreToken
      && pane.threadId === restoreThreadId
      && scrollContainer !== undefined
    );
    try {
      const result = await pane.loadOlder();
      await tick();
      if (!scrollContainer) return;
      if (anchor?.kind === 'anchor' && shouldContinue()) {
        const restored = await restoreLoadedAnchorSnapshot(anchor, shouldContinue);
        if (restored) {
          handleScroll();
          return;
        }
      }
      if (result.insertedRows || result.insertedBeforeWindow) {
        const delta = scrollContainer.scrollHeight - prevScrollHeight;
        scrollContainer.scrollTop = prevScrollTop + delta;
      }
      // Sync `userPinnedToBottom` from the post-prepend scroll position
      // BEFORE we release the suppress flag. Svelte re-runs the
      // auto-scroll effect the moment `suppressBottomAutoScroll`
      // flips false; if userPinnedToBottom is still stale-`true` from
      // before the load (short thread whose window fit the viewport),
      // the effect would snap to the new bottom and stomp the row
      // the user was anchored on. Programmatic scrollTop assignment
      // queues a `scroll` event asynchronously — too late for this
      // effect run — so we recompute inline.
      handleScroll();
    } finally {
      releaseBottomAutoScroll();
    }
  }

  /**
   * Scroll the timeline to the inline row for `id`. Invoked only by
   * the `scrollToItemRequest` $effect below — external callers publish
   * the intent through `pane.requestScrollToItem` (search hits, plan
   * sidebar rows, tray rows, plan follow-up banners) so the DOM
   * operation stays inside the component that owns the scroll
   * container. The target may live below the loaded window; we ask
   * the pane to page back until it's in view before querying the DOM.
   * A `false` return from `loadUntilItem` means the backend couldn't
   * find the item on this thread — surface a toast instead of
   * silently failing.
   */
  async function scrollToItem(id: string): Promise<void> {
    if (!scrollContainer || !id) return;
    const found = await pane.loadUntilItem(id);
    if (!scrollContainer) return;
    if (!found) {
      addToast('warning', 'Message is no longer in this thread');
      return;
    }
    await tick();
    if (!scrollContainer) return;
    const targetIndex = groupedNodes.findIndex((node) => nodeContainsItem(node, id));
    if (targetIndex >= 0) {
      scrollContainer.scrollTop = Math.max(
        0,
        offsetForIndex(targetIndex) - Math.round(scrollContainer.clientHeight / 2),
      );
      handleScroll();
      await tick();
    }
    const el = scrollContainer.querySelector(`[data-item-id="${CSS.escape(id)}"]`);
    if (el instanceof HTMLElement) {
      el.scrollIntoView({ behavior: 'smooth', block: 'center' });
    }
  }

  /**
   * Observe pane-published scroll-to-item intents. The pane exposes a
   * `{itemId, nonce}` tuple; a bumped nonce is our cue to run
   * scrollToItem. Tracked imperatively rather than via a $derived so a
   * repeat publish (same id, new nonce) still triggers. Nonce 0 is the
   * initial state and we deliberately ignore it so mounting a pane
   * doesn't auto-scroll.
   */
  let lastHandledScrollNonce = 0;
  $effect(() => {
    const req = pane.scrollToItemRequest;
    if (req.nonce === 0) return;
    if (req.nonce === lastHandledScrollNonce) return;
    lastHandledScrollNonce = req.nonce;
    void scrollToItem(req.itemId);
  });
</script>

<div bind:this={scrollContainer} use:measureScrollContainer onscroll={handleScroll} class="flex-1 min-h-0 overflow-y-auto" role="log" aria-label="Message history" data-testid="message-timeline-scroll">
  <div class="mx-auto w-full max-w-3xl px-6 py-8">
  {#if pane.loading}
    <div class="flex items-center justify-center h-full text-fg-subtle text-sm" role="status" aria-live="polite">
      <span class="animate-pulse">Loading thread...</span>
    </div>
  {:else}
    {#snippet leafContent(item: Item, orphan: boolean)}
      <TimelineLeaf {pane} {item} {orphan} {onImageExpand} />
    {/snippet}

    {#snippet renderNode(node: TimelineNode, depth: number)}
      {#if node.kind === 'leaf'}
        {@render leafContent(node.item, node.orphan === true)}
      {:else}
        <SubagentGroup group={node} depth={depth} renderNode={renderNode} />
      {/if}
    {/snippet}

    {#if pane.hasMoreHistory}
      <!-- Paged history control. Sits above the timeline so it's always
           in view when the user scrolls to the top. Delta-based scroll
           preservation lives in handleLoadOlder. -->
      <div class="mb-3 flex justify-center">
        <Button
          variant="secondary"
          size="xs"
          onclick={handleLoadOlder}
          loading={pane.loadingOlder}
          testId="load-older-messages"
        >
          {#snippet children()}
            {pane.loadingOlder ? 'Loading…' : 'Load older messages'}
          {/snippet}
        </Button>
      </div>
    {/if}

    <div style:height={`${virtualWindow.before}px`} aria-hidden="true"></div>
    {#each virtualWindow.rows as row (row.key)}
      <div use:measureTimelineRow={{ key: row.key, estimatedHeight: row.estimatedHeight }}>
        {#if pane.latestSettledTurn && shouldRenderDividerBefore(row.node, pane.latestSettledTurn)}
          <!-- The divider renders BEFORE the assistant message that closed
               the settled turn (t3-code pattern). The per-turn diff summary
               + TurnDiffBadge render AFTER the turn's last root node below,
               so the two don't conflict — the divider sits above the final
               assistant message, the diff badge sits below it at the turn
               boundary. -->
          <CompletionDivider turn={pane.latestSettledTurn} />
        {/if}
        <!-- Root nodes feed in at depth=1 so SubagentGroup's GRANDCHILD
             cap aligns with the spec numbering (first card=1, child=2,
             grandchild=3 marker-only plateau). -->
        <div data-testid="message-timeline-node">
          {@render renderNode(row.node, 1)}
        </div>

        {#if isLastRootInTurn(row.index)}
          {@const turnIndex = rootTurnIndex(row.node)}
          {@const turnView = turnDiffViews.get(turnIndex)}
          {#if turnView && getSettings().showEndOfTurnDiffs}
            <ChangedFilesTree files={turnView.files} />
            {#if turnSummaryIsMeaningful(turnView.summary)}
              <TurnDiffBadge {pane} {turnIndex} summary={turnView.summary} />
            {/if}
          {/if}
        {/if}
      </div>
    {/each}
    <div style:height={`${virtualWindow.after}px`} aria-hidden="true"></div>

    <ChatWorkingIndicator {pane} />

    {#if pane.items.length === 0 && !pane.activeTurn}
      <div class="flex items-center justify-center h-full text-fg-subtle text-sm">
        No messages yet. Send a message to get started.
      </div>
    {/if}
  {/if}
  </div>
</div>
