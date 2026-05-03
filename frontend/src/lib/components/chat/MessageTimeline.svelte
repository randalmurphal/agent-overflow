<script lang="ts" module>
  // The snapshot store + helpers live in `utils/threadScrollSnapshots`.
  // Re-export the test helper so existing tests keep working without
  // chasing the new path.
  export { clearThreadScrollSnapshotsForTest as clearMessageTimelineScrollSnapshotsForTest } from '../../utils/threadScrollSnapshots';
</script>

<script lang="ts">
  import { onDestroy, tick } from 'svelte';
  import { Virtualizer, type VirtualizerHandle } from 'virtua/svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { createUseStickToBottomController } from '../../utils/useStickToBottom.svelte';
  import {
    getThreadScrollSnapshot,
    setThreadScrollSnapshot,
  } from '../../utils/threadScrollSnapshots';
  import {
    finalAssistantTextIdsByTurn,
    findTimelineNodeIndex as findNodeIndexInList,
    groupItemsBySubagent,
    isToolTextBoundary as isToolTextBoundaryAt,
    nodeRole,
    timelineNodeItemId,
    timelineNodeKey,
    timelineNodeTurnIndex,
    type TimelineNode,
  } from '../../utils/subagentGrouping';
  import { filterRedundantNotifications } from '../../utils/notificationFilter';
  import { getActiveTurn } from '../../stores/threadStatuses.svelte';
  import Button from '../primitives/Button.svelte';
  import InlineSubagentGroup from './InlineSubagentGroup.svelte';
  import ScrollToBottomButton from './ScrollToBottomButton.svelte';
  import SubagentGroup from './SubagentGroup.svelte';
  import TimelineLeaf from './TimelineLeaf.svelte';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import { recordTimelineRenderTrace } from './messageTimelineTrace';

  // Initial item-size estimate for virtua. Real sizes come from the
  // per-item ResizeObserver virtua wraps each row in; this constant only
  // matters for the first render before measurements stabilise.
  const ESTIMATED_ROW_SIZE = 90;
  // Extra buffer rendered above + below the viewport. Sized for two viewports
  // worth of rows on each side so fast scrolls (trackpad fling, scrollbar
  // drag) don't outrun the rendered window — that was the source of the
  // "text disappears under the composer then reappears" flicker. Each
  // ~90px row × 1800px = ~20 extra rows per side. Trade ~4MB of mounted
  // DOM/component state for the smoother scroll. Revisit only if it's
  // ever measured to hurt mount-time on first-open.
  const BUFFER_SIZE_PX = 1800;
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

  function shouldRenderTurnBoundaryBefore(index: number, node: TimelineNode): boolean {
    if (node.kind !== 'leaf' || node.item.kind !== 'assistant_text') return false;
    for (let i = index - 1; i >= 0; i -= 1) {
      const previous = groupedNodes[i];
      if (!previous) return false;
      if (previous.kind === 'leaf' && previous.item.turnIndex !== node.item.turnIndex) return false;
      if (previous.kind !== 'leaf' && timelineNodeTurnIndex(previous) !== node.item.turnIndex) return false;
      const previousRole = nodeRole(previous);
      if (previousRole === 'tool') return true;
      if (previousRole === 'text') return false;
    }
    return false;
  }

  // Inner scroll container. We own scrolling here; <Virtualizer> renders
  // its measured rows inside `contentEl` and reads/writes via scrollRef.
  // The element is wrapped in a non-scrolling `relative flex h-full
  // flex-col` shell that anchors the floating <ScrollToBottomButton>
  // outside the scroll content (see template comment for why).
  let scrollEl: HTMLDivElement | undefined = $state(undefined);
  // Wrapper around <Virtualizer>. The controller's content RO observes
  // this element so any size change (row growth, expand toggle, virtua's
  // totalSize bump) fires synchronously before the same paint that
  // displays the change.
  let contentEl: HTMLDivElement | undefined = $state(undefined);
  // Imperative handle into virtua. Set once Virtualizer mounts.
  let listRef: VirtualizerHandle | undefined = $state(undefined);

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

  let finalAssistantTextIds = $derived(
    finalAssistantTextIdsByTurn(groupedNodes, getActiveTurn(pane.threadId)?.turnIndex ?? null),
  );

  const stick = createUseStickToBottomController();

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

  // Bind the controller to the actual DOM elements. The content RO,
  // wheel/scroll/keydown/touch listeners, and spring driver all start
  // here. Re-runs if either ref changes (thread switch / HMR).
  $effect(() => {
    if (!scrollEl || !contentEl) return;
    stick.attach(scrollEl, contentEl);
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
  // Pure helpers live alongside the TimelineNode type in
  // subagentGrouping.ts; the local thin wrappers below adapt them to the
  // current groupedNodes array so the template doesn't have to thread
  // `groupedNodes` into every call site.

  function findTimelineNodeIndex(itemId: string): number {
    return findNodeIndexInList(groupedNodes, itemId);
  }

  function isToolTextBoundary(index: number): boolean {
    return isToolTextBoundaryAt(groupedNodes, index);
  }

  // ============================================================
  // Virtualizer scroll callbacks → snapshot persist
  // ============================================================
  // The native scroll listener bound by the controller drives intent.
  // Virtualizer's callbacks here are only for snapshot persistence so
  // back-button / thread-switch returns to the same place.

  function handleVirtuaScroll(_offset: number): void {
    saveScrollSnapshot();
  }

  function handleVirtuaScrollEnd(): void {
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
      if (stick.isAtBottom) {
        setThreadScrollSnapshot(threadId, { kind: 'bottom' });
        return;
      }
      const offset = listRef.getScrollOffset();
      const rawIdx = listRef.findItemIndex(offset);
      if (rawIdx < 0) return;
      const idx = Math.min(rawIdx, groupedNodes.length - 1);
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
  // effects run, AND suspend auto-follow until restoreInitialPosition
  // takes over.
  // Without this, virtua's per-row measurements grow contentEl over
  // multiple frames as it remeasures; the controller's content-RO sees
  // positive deltas and the spring chases the bottom — visible to the
  // user as a top→bottom scroll animation on every thread open. The
  // existing $effect that calls restoreInitialPosition runs only after
  // pane.loading flips false, which is too late: the spring has already
  // started. Setting escapedFromLockState=true synchronously here
  // short-circuits startSpringIfNeeded; restoreInitialPosition then
  // either calls forceStick({animation:'instant'}) (clears escape, snaps
  // to bottom) or stopScroll() + setEscapedFromLock(true) +
  // scrollToIndex() (keeps escape true, jumps to anchor) — both leave
  // the controller in the correct end state without any visible transit.
  $effect.pre(() => {
    const nextThreadId = pane.threadId;
    if (scrollSnapshotThreadId !== nextThreadId) {
      if (scrollSnapshotThreadId) {
        saveScrollSnapshotForThread(scrollSnapshotThreadId, true);
        restoredThreadId = null;
        restoreToken += 1;
      }
      stick.setEscapedFromLock(true);
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
        stick.forceStick({ animation: 'instant' });
        saveScrollSnapshot();
        return;
      }

      // Anchor snapshot: ensure the target item is loaded, then scroll
      // to it preserving the recorded offset within its row.
      const found = await pane.loadUntilItem(snap.itemId);
      if (token !== restoreToken || pane.threadId !== threadId || !listRef) return;
      if (!found) {
        stick.forceStick({ animation: 'instant' });
        saveScrollSnapshot();
        return;
      }
      await tick();
      if (token !== restoreToken || pane.threadId !== threadId || !listRef) return;
      const idx = findTimelineNodeIndex(snap.itemId);
      if (idx < 0) {
        stick.forceStick({ animation: 'instant' });
        saveScrollSnapshot();
        return;
      }
      // User wasn't at bottom when they left — explicit jump elsewhere
      // must not restick. stopScroll cancels any in-flight spring before
      // virtua's scrollToIndex writes scrollTop, then setEscapedFromLock
      // ensures the resulting near-bottom check doesn't auto-restick.
      stick.stopScroll();
      stick.setEscapedFromLock(true);
      listRef.scrollToIndex(idx, { align: 'start', offset: -snap.offsetTop });
      saveScrollSnapshot();
    } finally {
      release();
    }
  }

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
    // The user is reading older — must not auto-restick from the
    // post-prepend scrollHeight jump. Without this, the controller's
    // content RO would observe the positive delta and the spring would
    // yank the user to the new bottom.
    stick.setEscapedFromLock(true);
    const myToken = ++restoreToken;
    try {
      const result = await pane.loadOlder();
      await tick();
      if (myToken !== restoreToken || !listRef || result.status !== 'loaded') return;
      if (!anchorId) return;
      const newIdx = findTimelineNodeIndex(anchorId);
      if (newIdx < 0) return;
      stick.stopScroll();
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
    if (idx < 0) return;
    // Programmatic jump elsewhere — cancel spring + escape so the new
    // position holds. `smooth: true` would route through the browser's
    // native smooth scroll (scrollEl.scrollTo({behavior:'smooth'})),
    // which would race the spring driver — drop it.
    stick.stopScroll();
    stick.setEscapedFromLock(true);
    listRef.scrollToIndex(idx, { align: 'center' });
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
    stick.detach();
  });
</script>

<!-- Outer wrapper does NOT scroll; it provides the relative containing
     block for the floating scroll-to-bottom chip. The chip MUST sit
     outside the scroll container: `position:absolute; bottom:X` inside
     an `overflow:auto` parent renders in scroll-content space, not
     viewport space, so the chip would otherwise scroll with the
     transcript and ride up off-screen as the user scrolls or as
     auto-follow drives scrollTop downward (browser-confirmed). The
     inner div is the actual scroll container that <Virtualizer
     scrollRef={scrollEl}> reads/writes via, that the controller's
     wheel/scroll/keydown/touch listeners and content ResizeObserver
     bind to. The padding-bottom (= composer height + visual breathing
     room) keeps the last message clear of the absolute composer
     overlay without putting a synthetic spacer row inside the
     virtualized data. Layout shape mirrors discussion/ChannelView.svelte
     (`relative flex h-full flex-col` + `flex-1 min-h-0 overflow-y-auto`)
     so the two surfaces stay in lockstep. -->
<div class="relative flex h-full flex-col">
  <div
    bind:this={scrollEl}
    class="flex-1 min-h-0 overflow-y-auto"
    style:overscroll-behavior-y="contain"
    style:padding-bottom={`calc(var(--composer-height, 0px) + ${BOTTOM_PAD_PX}px)`}
    tabindex="-1"
    data-testid="message-timeline-scroll"
    role="log"
    aria-label="Message History"
  >
    {#if pane.loading}
      <div class="flex items-center justify-center h-full text-fg-subtle text-sm" role="status" aria-live="polite">
        <span class="animate-pulse">Loading thread...</span>
      </div>
    {:else if pane.items.length === 0 && !getActiveTurn(pane.threadId)}
      <div class="flex items-center justify-center h-full text-fg-subtle text-sm">
        No messages yet. Send a message to get started.
      </div>
    {:else if pane.items.length === 0 && getActiveTurn(pane.threadId)}
      <!-- Active turn but no items yet. The working/todo UI lives in the
           ChatView bottom overlay, outside the virtualized history. -->
      <div class="mx-auto w-full max-w-[62rem] px-6 pt-8"></div>
    {:else}
      {#snippet renderNode(node: TimelineNode, depth: number)}
        {#if node.kind === 'leaf'}
          <TimelineLeaf {pane} item={node.item} orphan={node.orphan === true} {onImageExpand} />
        {:else if node.kind === 'group'}
          <SubagentGroup {pane} group={node} {depth} {renderNode} />
        {:else}
          <InlineSubagentGroup group={node} {depth} {renderNode} />
        {/if}
      {/snippet}

      <!-- contentEl is the controller's content-RO observation target.
           Virtua's container has `contain: size; height: totalSize+'px'`,
           so contentEl.scrollHeight reflects virtua's totalSize exactly. -->
      <div bind:this={contentEl}>
        <Virtualizer
          bind:this={listRef}
          scrollRef={scrollEl}
          data={groupedNodes}
          getKey={(node) => timelineNodeKey(node)}
          itemSize={ESTIMATED_ROW_SIZE}
          bufferSize={BUFFER_SIZE_PX}
          ssrCount={IS_TEST ? 100_000 : undefined}
          onscroll={handleVirtuaScroll}
          onscrollend={handleVirtuaScrollEnd}
        >
          {#snippet children(node: TimelineNode, index: number)}
            <!-- Outer per-row wrapper. We do NOT set data-item-id here:
                 only TimelineLeaf owns that attribute on its root. Structural
                 rows stay unanchored, and tests rely on the divider rendering
                 BEFORE the [data-item-id] node, not containing it. -->
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
                {#if shouldRenderTurnBoundaryBefore(index, node)}
                  {@const showResponsePill = node.kind === 'leaf' && finalAssistantTextIds.has(node.item.id)}
                  <!-- Two visual modes share a fixed wrapper height
                       (`h-[1.625rem]` = 26px = pill chrome: text-[10px]
                       × leading-tight ≈ 12px + py-1 8px + 2× 1px border).
                       Labeled mode renders `line | gap | pill | gap | line`,
                       unlabeled mode renders one continuous full-width line.
                       The pill's `leading-tight` keeps its content inside
                       the fixed wrapper across font-loading variance.
                       Fixed `h-` (not the codebase's usual `min-h-` slot
                       convention) is deliberate: both branches MUST be the
                       exact same height so promoting an intermediate
                       divider to "final" on turn settle never shifts row
                       geometry — satisfies the "no late transcript
                       adornments on completion" rule in
                       `frontend/CLAUDE.md`. Re-derive 1.625rem if the pill
                       classes above change. -->
                  <div data-testid="response-divider" data-final-response={showResponsePill ? 'true' : 'false'}>
                    <div class="my-3 flex h-[1.625rem] items-center gap-3">
                      <span class="h-px flex-1 bg-border" aria-hidden="true"></span>
                      {#if showResponsePill}
                        <span
                          class="rounded-full border border-border bg-surface-1 px-2.5 py-1 text-[10px] uppercase leading-tight tracking-[0.14em] text-text-secondary"
                        >
                          Response
                        </span>
                        <span class="h-px flex-1 bg-border" aria-hidden="true"></span>
                      {/if}
                    </div>
                  </div>
                {/if}
                <div data-testid="message-timeline-node">
                  {@render renderNode(node, 1)}
                </div>
              </div>
            </div>
          {/snippet}
        </Virtualizer>
      </div>
    {/if}
  </div>

  <!-- Visible when NOT at bottom (intent-or-geometry); the chip is the
       user's escape hatch when they've drifted away. Wiring this to
       `!isSticky` would also pop the chip during sidebar/drawer resize
       leases (pauseDepth > 0) even though the user is geometrically
       glued to the bottom — clearly not desired. Anchored to the outer
       wrapper (which does not scroll), so the chip stays fixed in the
       visible area regardless of transcript scrollTop. -->
  <ScrollToBottomButton visible={!stick.isAtBottom} onClick={() => stick.forceStick()} />
</div>
