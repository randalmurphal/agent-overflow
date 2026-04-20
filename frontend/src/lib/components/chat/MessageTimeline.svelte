<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Item } from '../../types/models';
  import { groupItemsBySubagent, type TimelineNode } from '../../utils/subagentGrouping';
  import { turnSummaryIsMeaningful } from '../../utils/turnDiffSummary';
  import AssistantMessage from './AssistantMessage.svelte';
  import ChangedFilesTree from './ChangedFilesTree.svelte';
  import SubagentGroup from './SubagentGroup.svelte';
  import ThinkingBlock from './ThinkingBlock.svelte';
  import ToolCallCard from './ToolCallCard.svelte';
  import TurnDiffBadge from './TurnDiffBadge.svelte';
  import UserMessage from './UserMessage.svelte';

  let { pane }: { pane: ThreadPane } = $props();

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

  // Auto-scroll only when the user is already near the bottom.
  $effect(() => {
    pane.items.length;

    if (scrollContainer && userNearBottom) {
      requestAnimationFrame(() => {
        if (!scrollContainer) return;
        scrollContainer.scrollTop = scrollContainer.scrollHeight;
      });
    }
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

    {#each groupedNodes as node, index (node.kind === 'group' ? `g:${node.parent.id}` : `l:${node.item.id}`)}
      <!-- content-visibility: auto lets the browser skip paint + layout for
           off-screen nodes. Pairs with contain-intrinsic-size as a layout
           placeholder so scroll height stays sane when nodes aren't measured
           yet. This is the spec-sanctioned "virtualize the whole list"
           approach (no count-slicing, no anchor IDs) — every node stays in
           the DOM with stable identity, but only the visible ones pay the
           render cost. The intrinsic size is a rough average; the real
           height replaces it once the node scrolls into view. -->
      <div
        data-testid="message-timeline-node"
        class="contents-visibility-auto"
      >
        <!-- Root nodes feed in at depth=1 so SubagentGroup's GRANDCHILD
             cap aligns with the spec numbering (first card=1, child=2,
             grandchild=3 marker-only plateau). -->
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
