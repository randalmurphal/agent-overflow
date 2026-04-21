<script lang="ts">
  import { tick } from 'svelte';
  import type { SettledTurn, ThreadPane } from '../../stores/thread.svelte';
  import type { Item } from '../../types/models';
  import { addToast } from '../../stores/toast.svelte';
  import { groupItemsBySubagent, type TimelineNode } from '../../utils/subagentGrouping';
  import { turnSummaryIsMeaningful } from '../../utils/turnDiffSummary';
  import AssistantMessage from './AssistantMessage.svelte';
  import ChangedFilesTree from './ChangedFilesTree.svelte';
  import CompletionDivider from './CompletionDivider.svelte';
  import SubagentGroup from './SubagentGroup.svelte';
  import ThinkingBlock from './ThinkingBlock.svelte';
  import ToolCallCard from './ToolCallCard.svelte';
  import TurnDiffBadge from './TurnDiffBadge.svelte';
  import UserMessage from './UserMessage.svelte';

  let { pane }: { pane: ThreadPane } = $props();

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

  /**
   * Track whether the user is near the bottom of the scroll container.
   * Only auto-scroll when the user hasn't scrolled away from bottom.
   */
  let userNearBottom = $state(true);
  const NEAR_BOTTOM_THRESHOLD = 100; // px

  function handleScroll() {
    if (!scrollContainer) return;
    const { scrollTop, scrollHeight, clientHeight } = scrollContainer;
    userNearBottom = scrollHeight - scrollTop - clientHeight <= NEAR_BOTTOM_THRESHOLD;
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

  /**
   * Turn boundaries on the root-node stream: we emit the ChangedFilesTree
   * summary at the end of every turn that has diff activity. Subagent
   * children share their parent's turnIndex (triage uses LastTurnIndex for
   * child persistence), so the turn of a group node is the parent's turn.
   */
  function rootTurnIndex(node: TimelineNode): number {
    return node.kind === 'leaf' ? node.item.turnIndex : node.parent.turnIndex;
  }

  function isLastRootInTurn(index: number): boolean {
    const current = groupedNodes[index];
    const next = groupedNodes[index + 1];
    if (!current) return false;
    if (!next) return true;
    return rootTurnIndex(current) !== rootTurnIndex(next);
  }

  /**
   * Suppress the "stick to bottom" auto-scroll while a Load Older
   * round-trip is in flight. Without this, a short thread whose whole
   * window fits in the viewport keeps `userNearBottom` true throughout
   * the prepend. When items.length grows, the effect would scroll the
   * container to the new bottom a frame after our handleLoadOlder
   * delta-apply — snapping the user past the newly revealed history.
   */
  let suppressBottomAutoScroll = $state(false);

  // Auto-scroll only when the user is already near the bottom.
  $effect(() => {
    pane.items.length;

    if (suppressBottomAutoScroll) return;
    if (scrollContainer && userNearBottom) {
      requestAnimationFrame(() => {
        if (!scrollContainer) return;
        scrollContainer.scrollTop = scrollContainer.scrollHeight;
      });
    }
  });

  /**
   * Fetch older history and preserve the user's visual anchor row. We
   * capture the pre-prepend scrollHeight/scrollTop, wait for the store
   * to prepend + rebuild turn diff views, then await a tick so the DOM
   * reflects the new rows. The growth delta is re-applied to scrollTop
   * so the row the user was reading stays at the same viewport
   * position. `suppressBottomAutoScroll` is raised across the await so
   * the near-bottom auto-scroll effect can't fire on the
   * items-length-change and snap past the newly revealed rows — short
   * threads whose window already fits the viewport would otherwise
   * keep `userNearBottom` true throughout the prepend.
   */
  async function handleLoadOlder(): Promise<void> {
    if (!scrollContainer) return;
    const prevScrollHeight = scrollContainer.scrollHeight;
    const prevScrollTop = scrollContainer.scrollTop;
    suppressBottomAutoScroll = true;
    try {
      await pane.loadOlder();
      await tick();
      if (!scrollContainer) return;
      const delta = scrollContainer.scrollHeight - prevScrollHeight;
      scrollContainer.scrollTop = prevScrollTop + delta;
      // Sync `userNearBottom` from the post-prepend scroll position
      // BEFORE we release the suppress flag. Svelte re-runs the
      // auto-scroll effect the moment `suppressBottomAutoScroll`
      // flips false; if userNearBottom is still stale-`true` from
      // before the load (short thread whose window fit the viewport),
      // the effect would snap to the new bottom and stomp the row
      // the user was anchored on. Programmatic scrollTop assignment
      // queues a `scroll` event asynchronously — too late for this
      // effect run — so we recompute inline.
      handleScroll();
    } finally {
      suppressBottomAutoScroll = false;
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

<div bind:this={scrollContainer} onscroll={handleScroll} class="flex-1 overflow-y-auto px-4 py-4" role="log" aria-label="Message history">
  {#if pane.loading}
    <div class="flex items-center justify-center h-full text-text-secondary text-sm" role="status" aria-live="polite">
      <span class="animate-pulse">Loading thread...</span>
    </div>
  {:else}
    {#snippet leafContent(item: Item, orphan: boolean)}
      <div data-item-id={item.id}>
        {#if orphan}
          <div
            class="mb-1 flex items-center gap-2 text-xs text-warning"
            role="status"
            aria-label="Orphan subagent item"
          >
            <span aria-hidden="true">⚠</span>
            <span>Orphan subagent entry — parent tool call not found.</span>
          </div>
        {/if}
        {#if item.kind === 'user_text'}
          <UserMessage {item} />
        {:else if item.kind === 'tool_call' || item.kind === 'tool_completion'}
          <ToolCallCard {pane} {item} />
        {:else if item.kind === 'thinking'}
          <ThinkingBlock {item} />
        {:else if item.kind === 'error'}
          <div class="mb-3 rounded border border-error/30 bg-error/10 px-3 py-2 text-sm text-error">
            {item.summary}
          </div>
        {:else if item.kind === 'compaction'}
          <div class="mb-3 flex items-center gap-3 text-xs uppercase tracking-wide text-text-secondary">
            <div class="h-px flex-1 bg-border"></div>
            <span>{item.summary || 'Context compacted'}</span>
            <div class="h-px flex-1 bg-border"></div>
          </div>
        {:else}
          <AssistantMessage {item} />
        {/if}
      </div>
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
      <div class="mb-2 flex justify-center">
        <button
          data-testid="load-older-messages"
          class="rounded border border-border px-3 py-1.5 text-sm text-text-secondary hover:text-text-primary disabled:opacity-50"
          disabled={pane.loadingOlder}
          onclick={handleLoadOlder}
        >
          {pane.loadingOlder ? 'Loading…' : 'Load older messages'}
        </button>
      </div>
    {/if}

    {#each groupedNodes as node, index (node.kind === 'group' ? `g:${node.parent.id}` : `l:${node.item.id}`)}
      {#if pane.latestSettledTurn && shouldRenderDividerBefore(node, pane.latestSettledTurn)}
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
        {@render renderNode(node, 1)}
      </div>

      {#if isLastRootInTurn(index)}
        {@const turnIndex = rootTurnIndex(node)}
        {@const turnView = turnDiffViews.get(turnIndex)}
        {#if turnView}
          <ChangedFilesTree files={turnView.files} />
          {#if turnSummaryIsMeaningful(turnView.summary)}
            <TurnDiffBadge {pane} {turnIndex} summary={turnView.summary} />
          {/if}
        {/if}
      {/if}
    {/each}

    {#if pane.items.length === 0}
      <div class="flex items-center justify-center h-full text-text-secondary text-sm">
        No messages yet. Send a message to get started.
      </div>
    {/if}
  {/if}
</div>
