<script lang="ts" module>
  // The snapshot store + helpers live in `utils/threadScrollSnapshots`.
  // Re-export the test helper so existing tests keep working without
  // chasing the new path.
  export { clearThreadScrollSnapshotsForTest as clearMessageTimelineScrollSnapshotsForTest } from '../../utils/threadScrollSnapshots';
</script>

<script lang="ts">
  import { onDestroy, tick } from 'svelte';
  import { VList, type VListHandle } from 'virtua/svelte';
  import type { SettledTurn, ThreadPane } from '../../stores/thread.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { createStickyBottomController } from '../../utils/stickyBottomController.svelte';
  import {
    getThreadScrollSnapshot,
    setThreadScrollSnapshot,
  } from '../../utils/threadScrollSnapshots';
  import {
    findTimelineNodeIndex as findNodeIndexInList,
    groupItemsBySubagent,
    isLastRootInTurn as isLastRootInTurnAt,
    isToolTextBoundary as isToolTextBoundaryAt,
    rootTurnIndex,
    timelineNodeItemId,
    timelineNodeKey,
    type TimelineNode,
  } from '../../utils/subagentGrouping';
  import { filterRedundantNotifications } from '../../utils/notificationFilter';
  import Button from '../primitives/Button.svelte';
  import { turnSummaryIsMeaningful } from '../../utils/turnDiffSummary';
  import ChangedFilesTree from './ChangedFilesTree.svelte';
  import ChatWorkingIndicator from './ChatWorkingIndicator.svelte';
  import LiveTodoPanel from './LiveTodoPanel.svelte';
  import CompletionDivider from './CompletionDivider.svelte';
  import ScrollToBottomButton from './ScrollToBottomButton.svelte';
  import SubagentGroup from './SubagentGroup.svelte';
  import TimelineLeaf from './TimelineLeaf.svelte';
  import TurnDiffBadge from './TurnDiffBadge.svelte';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import { recordTimelineRenderTrace } from './messageTimelineTrace';

  // Initial item-size estimate for virtua. Real sizes come from the
  // per-item ResizeObserver virtua wraps each row in; this constant only
  // matters for the first render before measurements stabilise.
  const ESTIMATED_ROW_SIZE = 90;
  // Extra buffer rendered above + below the viewport. Larger than virtua's
  // default 200px so the row at the user's anchor position is reliably in
  // the DOM during scroll, which keeps gesture-driven anchoring smooth.
  const BUFFER_SIZE_PX = 900;
  // Visual breathing room between the last message and the composer
  // overlay; combined with the --composer-height variable from ChatView.
  const BOTTOM_PAD_PX = 24;
  // happy-dom returns 0 for clientHeight/clientWidth, which makes virtua
  // mount zero rows. In test runs we ask virtua to mount everything via
  // ssrCount so test assertions can find the rendered DOM. Production
  // (vite dev/build) always sees the default `undefined`, leaving virtua
  // free to virtualize.
  const IS_TEST = import.meta.env.MODE === 'test';

  let {
    pane,
    onImageExpand,
  }: {
    pane: ThreadPane;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
  } = $props();

  /**
   * Decide whether the completion divider renders immediately before this
   * node in the timeline flow:
   *   - there's a settled turn to render for
   *   - that turn reported a terminal assistant message id
   *   - the current node is the leaf whose id matches that message
   * Subagent-group nodes and non-matching leaves render nothing. Only
   * leaves carry `data-item-id`, so the divider can only anchor before a
   * leaf — group nodes are structural and never trigger a divider.
   *
   * Spec: docs/architecture/turn-lifecycle.md §UI components driven by
   * this state.
   */
  function shouldRenderDividerBefore(
    node: TimelineNode,
    turn: SettledTurn | null,
  ): boolean {
    if (!turn) return false;
    if (!turn.assistantMessageId) return false;
    if (node.kind !== 'leaf') return false;
    return node.item.id === turn.assistantMessageId;
  }

  // The wrapper div around <VList> that catches gesture events for the
  // sticky controller. virtua owns the inner scroll element; we attach
  // listeners on the outer wrapper and let events bubble.
  let scrollEl: HTMLDivElement | undefined = $state(undefined);
  // Imperative handle into virtua. Set once VList mounts.
  let listRef: VListHandle | undefined = $state(undefined);

  let restoredThreadId: string | null = $state(null);
  // Tracks which thread we last persisted into the snapshot store via
  // the thread-switch effect — separate from `restoredThreadId` so a
  // thread switch can dispose the previous snapshot before the next
  // restore completes.
  let scrollSnapshotThreadId: string | null = $state(null);
  // Token bumped on every external "interrupt" — thread switch, user
  // scroll, programmatic scrollToItem — so async restore work can detect
  // staleness and bail.
  let restoreToken = 0;

  let groupedNodes = $derived<TimelineNode[]>(
    groupItemsBySubagent(filterRedundantNotifications(pane.items)),
  );
  let turnDiffViews = $derived(pane.turnDiffViews);

  const stick = createStickyBottomController({
    getScrollEl: () => scrollEl,
    getListHandle: () => listRef,
    getLastIndex: () => groupedNodes.length - 1,
  });

  // Publish the controller on the pane so external surfaces (sidebar
  // resizers, resizable drawers) can acquire a `pauseAutoScroll()` lease
  // during gestures. The effect's return function detaches symmetrically
  // when the pane reference changes (rare — ChatView re-keys on thread
  // switch, which remounts the timeline) and on component teardown, so
  // a stale pointer to a torn-down controller can't leak.
  $effect(() => {
    pane.attachScrollController(stick);
    return () => pane.detachScrollController(stick);
  });

  $effect(() => {
    void scrollEl;
    void listRef;
    stick.attach();
  });

  // Diagnostic UI render trace — extracted to messageTimelineTrace.ts.
  // Production builds short-circuit at isUiRenderTraceEnabled() inside
  // the helper, so this $effect's only steady-state cost is the reactive
  // dep tracking.
  $effect(() => {
    pane.threadId;
    pane.items.length;
    pane.timelineRevision;
    groupedNodes.length;
    recordTimelineRenderTrace(pane, groupedNodes, scrollEl, listRef);
  });

  // ============================================================
  // Helpers
  // ============================================================
  // Pure helpers (rootTurnIndex, isLastRootInTurn, findTimelineNodeIndex,
  // timelineNodeItemId) live alongside the TimelineNode type in
  // subagentGrouping.ts; the local thin wrappers below adapt them to the
  // current groupedNodes array so the template doesn't have to thread
  // `groupedNodes` into every call site.

  function isLastRootInTurn(index: number): boolean {
    return isLastRootInTurnAt(groupedNodes, index);
  }

  function findTimelineNodeIndex(itemId: string): number {
    return findNodeIndexInList(groupedNodes, itemId);
  }

  function isToolTextBoundary(index: number): boolean {
    return isToolTextBoundaryAt(groupedNodes, index);
  }

  // ============================================================
  // VList scroll callbacks → controller
  // ============================================================

  function handleListScroll(offset: number): void {
    stick.onScroll(offset);
    saveScrollSnapshot();
  }

  function handleListScrollEnd(): void {
    stick.onScrollEnd();
    saveScrollSnapshot();
  }

  // ============================================================
  // Snapshot save/restore (per-thread)
  // ============================================================

  function snapshotThreadId(): string | null {
    return pane.threadId || null;
  }

  function saveScrollSnapshot(): void {
    const threadId = snapshotThreadId();
    if (!threadId) return;
    saveScrollSnapshotForThread(threadId, false);
  }

  function saveScrollSnapshotForThread(threadId: string, ignoreLoading: boolean): void {
    if (!listRef || (!ignoreLoading && pane.loading) || restoredThreadId !== threadId) return;
    // virtua's internal ref can be in a teardown state where any geometry
    // read throws (the inner ref is null while our outer handle is still
    // bound). The TypeError is the documented teardown shape — swallow
    // exactly that and re-throw anything else so a real regression in a
    // future virtua version doesn't disappear silently.
    try {
      if (stick.isAtBottom()) {
        setThreadScrollSnapshot(threadId, { kind: 'bottom' });
        return;
      }
      const offset = listRef.getScrollOffset();
      const idx = listRef.findItemIndex(offset);
      if (idx < 0) return;
      const node = groupedNodes[idx];
      if (!node) return;
      const itemId = timelineNodeItemId(node);
      // Negative when the anchor row's top has scrolled above the viewport
      // top by `-offsetTop` pixels. Restoration recreates exactly this
      // relationship via scrollToIndex({ align:'start', offset: -offsetTop }).
      const offsetTop = listRef.getItemOffset(idx) - offset;
      setThreadScrollSnapshot(threadId, { kind: 'anchor', itemId, offsetTop });
    } catch (err) {
      if (err instanceof TypeError) {
        // Expected during teardown when virtua's inner ref is nulled
        // while our outer handle is still bound. Skip this save; the
        // next scroll will refresh the snapshot.
        return;
      }
      throw err;
    }
  }

  // Persist + reset state on thread change BEFORE the new thread's
  // effects run. Mirrors the old thread-switch effect.pre.
  $effect.pre(() => {
    const nextThreadId = pane.threadId;
    if (scrollSnapshotThreadId && scrollSnapshotThreadId !== nextThreadId) {
      saveScrollSnapshotForThread(scrollSnapshotThreadId, true);
      restoredThreadId = null;
      restoreToken += 1;
    }
    scrollSnapshotThreadId = nextThreadId;
  });

  $effect(() => {
    const threadId = pane.threadId;
    const loading = pane.loading;
    if (!threadId || loading) return;
    if (restoredThreadId === threadId) return;
    restoredThreadId = threadId;
    void restoreInitialPosition(threadId, ++restoreToken);
  });

  async function restoreInitialPosition(threadId: string, token: number): Promise<void> {
    const release = stick.pauseAutoScroll();
    try {
      // Let virtua mount with the current data so listRef is populated.
      await tick();
      if (token !== restoreToken || pane.threadId !== threadId || !listRef) return;

      const snap = getThreadScrollSnapshot(threadId);
      if (!snap || snap.kind === 'bottom') {
        stick.forceStick();
        saveScrollSnapshot();
        return;
      }

      // Anchor snapshot: ensure the target item is loaded, then scroll
      // to it preserving the recorded offset within its row.
      const found = await pane.loadUntilItem(snap.itemId);
      if (token !== restoreToken || pane.threadId !== threadId || !listRef) return;
      if (!found) {
        stick.forceStick();
        saveScrollSnapshot();
        return;
      }
      await tick();
      if (token !== restoreToken || pane.threadId !== threadId || !listRef) return;
      const idx = findTimelineNodeIndex(snap.itemId);
      if (idx < 0) {
        stick.forceStick();
        saveScrollSnapshot();
        return;
      }
      listRef.scrollToIndex(idx, { align: 'start', offset: -snap.offsetTop });
      // After programmatic scroll, intent should be 'free' since the user
      // wasn't at the bottom when they left the thread. Don't force-stick.
      saveScrollSnapshot();
    } finally {
      release();
    }
  }

  // ============================================================
  // Auto-follow on growth
  // ============================================================
  // virtua's per-row ResizeObserver absorbs above-viewport height changes
  // silently. For below-viewport / append growth, the controller decides
  // whether to follow based on intent. Tracks length, revision, the
  // active turn, AND liveDeltaRevision so streaming chunks (which grow an
  // existing row in place without bumping items.length or timelineRevision)
  // still re-pin to the new bottom while sticky.
  $effect(() => {
    pane.items.length;
    pane.timelineRevision;
    pane.liveDeltaRevision;
    pane.activeTurn?.turnId;
    if (pane.threadId !== restoredThreadId) return;
    stick.notifyContentMaybeGrew();
  });

  // ============================================================
  // Load older
  // ============================================================
  // Capture the user's reading position BEFORE the await, then explicitly
  // re-anchor after the items prepend. Avoids virtua's `shift` mode (which
  // is documented as fragile across async boundaries) — explicit
  // scrollToIndex is deterministic.
  async function handleLoadOlder(): Promise<void> {
    if (!listRef) return;
    const offsetBefore = listRef.getScrollOffset();
    const firstIdxBefore = listRef.findItemIndex(offsetBefore);
    const node = groupedNodes[firstIdxBefore];
    const anchorId = node ? timelineNodeItemId(node) : null;
    const anchorOffsetTop = node ? listRef.getItemOffset(firstIdxBefore) - offsetBefore : 0;

    const release = stick.pauseAutoScroll();
    const myToken = ++restoreToken;
    try {
      const result = await pane.loadOlder();
      await tick();
      if (myToken !== restoreToken || !listRef || result.status !== 'loaded') return;
      if (!anchorId) return;
      const newIdx = findTimelineNodeIndex(anchorId);
      if (newIdx < 0) return;
      listRef.scrollToIndex(newIdx, { align: 'start', offset: -anchorOffsetTop });
      saveScrollSnapshot();
    } finally {
      release();
    }
  }

  // ============================================================
  // Scroll-to-item (search hits, plan rows, tray rows)
  // ============================================================

  async function scrollToItem(id: string): Promise<void> {
    if (!listRef || !id) return;
    const myToken = ++restoreToken;
    const found = await pane.loadUntilItem(id);
    if (myToken !== restoreToken || !listRef) return;
    if (!found) {
      addToast('warning', 'Message is no longer in this thread');
      return;
    }
    await tick();
    if (myToken !== restoreToken || !listRef) return;
    const idx = findTimelineNodeIndex(id);
    if (idx >= 0) listRef.scrollToIndex(idx, { align: 'center', smooth: true });
  }

  let lastHandledScrollNonce = 0;
  $effect(() => {
    const req = pane.scrollToItemRequest;
    if (req.nonce === 0) return;
    if (req.nonce === lastHandledScrollNonce) return;
    lastHandledScrollNonce = req.nonce;
    void scrollToItem(req.itemId);
  });

  onDestroy(() => {
    if (restoredThreadId) saveScrollSnapshotForThread(restoredThreadId, true);
    // Controller detach is handled by the registration $effect's return
    // function; we only need to dispose the controller's own listeners.
    stick.destroy();
  });
</script>

<!-- Outer wrapper: catches gesture events for the sticky controller and
     carries the timeline test id stably across loading / empty / populated
     states. The inner VList owns the actual scroll element. -->
<div bind:this={scrollEl} class="relative h-full" tabindex="-1" data-testid="message-timeline-scroll">
  {#if pane.loading}
    <div class="flex items-center justify-center h-full text-fg-subtle text-sm" role="status" aria-live="polite">
      <span class="animate-pulse">Loading thread...</span>
    </div>
  {:else if pane.items.length === 0 && !pane.activeTurn}
    <div class="flex items-center justify-center h-full text-fg-subtle text-sm">
      No messages yet. Send a message to get started.
    </div>
  {:else if pane.items.length === 0 && pane.activeTurn}
    <!-- Active turn but no items yet (just-sent prompt with the assistant
         not having streamed a single chunk). Render the working indicator
         standalone so the user sees feedback. -->
    <div class="mx-auto w-full max-w-[62rem] px-6 pt-8" style:padding-bottom={`calc(var(--composer-height, 0px) + ${BOTTOM_PAD_PX}px)`}>
      <ChatWorkingIndicator {pane} />
      <LiveTodoPanel {pane} />
    </div>
  {:else}
    {#snippet renderNode(node: TimelineNode, depth: number)}
      {#if node.kind === 'leaf'}
        <TimelineLeaf {pane} item={node.item} orphan={node.orphan === true} {onImageExpand} />
      {:else}
        <SubagentGroup {pane} group={node} {depth} {renderNode} />
      {/if}
    {/snippet}

    <VList
      bind:this={listRef}
      data={groupedNodes}
      getKey={(node) => timelineNodeKey(node)}
      itemSize={ESTIMATED_ROW_SIZE}
      bufferSize={BUFFER_SIZE_PX}
      ssrCount={IS_TEST ? 100_000 : undefined}
      onscroll={handleListScroll}
      onscrollend={handleListScrollEnd}
      style="height: 100%; overflow-x: hidden; overscroll-behavior-y: contain;"
      role="log"
      aria-label="Message History"
    >
      {#snippet children(node: TimelineNode, index: number)}
        <!-- Outer per-row wrapper. We do NOT set data-item-id here:
             TimelineLeaf and SubagentGroup own that attribute on their
             own roots, and tests rely on the divider rendering BEFORE
             the [data-item-id] node, not containing it. -->
        <div data-row-index={index} class:mt-4={isToolTextBoundary(index)}>
          {#if index === 0}
            <!-- Top of timeline. Load-older button (when applicable) and
                 a small top breathing-room spacer ride inside the first
                 row. When user scrolls to the very top, the button is
                 the first thing they see. After load-older completes,
                 the explicit scrollToIndex re-anchors them to where they
                 were reading — the button moves up out of view. -->
            <div class="pt-6 mx-auto w-full max-w-[62rem] px-6">
              {#if pane.hasMoreHistory}
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
            </div>
          {/if}

          <div class="mx-auto w-full max-w-[62rem] px-6">
            {#if pane.latestSettledTurn && shouldRenderDividerBefore(node, pane.latestSettledTurn)}
              <CompletionDivider turn={pane.latestSettledTurn} />
            {/if}
            <div data-testid="message-timeline-node">
              {@render renderNode(node, 1)}
            </div>
            {#if isLastRootInTurn(index)}
              {@const turnIndex = rootTurnIndex(node)}
              {@const turnView = turnDiffViews.get(turnIndex)}
              {#if turnView && getSettings().showEndOfTurnDiffs}
                <ChangedFilesTree files={turnView.files} />
                {#if turnSummaryIsMeaningful(turnView.summary)}
                  <TurnDiffBadge {pane} {turnIndex} summary={turnView.summary} />
                {/if}
              {/if}
            {/if}

            {#if index === groupedNodes.length - 1}
              <!-- Tail of timeline. Working indicator + LiveTodoPanel +
                   bottom spacer that consumes --composer-height so the
                   last visible row clears the absolute composer overlay
                   above it. The todo panel renders independently of the
                   working indicator (it persists past turn-end if items
                   remain incomplete and auto-hides on all-complete). -->
              <ChatWorkingIndicator {pane} />
              <LiveTodoPanel {pane} />
              <div
                aria-hidden="true"
                style:height={`calc(var(--composer-height, 0px) + ${BOTTOM_PAD_PX}px)`}
              ></div>
            {/if}
          </div>
        </div>
      {/snippet}
    </VList>
  {/if}

  <ScrollToBottomButton visible={!stick.isSticky} onClick={() => stick.forceStick()} />
</div>
