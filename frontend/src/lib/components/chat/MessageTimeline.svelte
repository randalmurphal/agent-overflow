<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ChangedFile, CommandOutputMeta, DiffMeta, Item, ProposedPlanMeta, ToolResultMeta } from '../../types/models';
  import { groupItemsBySubagent, type TimelineNode } from '../../utils/subagentGrouping';
  import { summarizeTurnDiffs, turnSummaryIsMeaningful, type TurnDiffSummary } from '../../utils/turnDiffSummary';
  import AssistantMessage from './AssistantMessage.svelte';
  import ChangedFilesTree from './ChangedFilesTree.svelte';
  import CommandOutput from './CommandOutput.svelte';
  import DiffPreview from './DiffPreview.svelte';
  import ProposedPlanCard from './ProposedPlanCard.svelte';
  import SubagentGroup from './SubagentGroup.svelte';
  import ThinkingBlock from './ThinkingBlock.svelte';
  import ToolResultCard from './ToolResultCard.svelte';
  import ToolResultDropdown from './ToolResultDropdown.svelte';
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

  function parseMeta<T>(item: Item | undefined): T | null {
    const raw = item?.payloadMeta;
    if (!raw) return null;
    try {
      return JSON.parse(raw) as T;
    } catch (err) {
      console.error('Failed to parse payload meta:', item?.id, err);
      return null;
    }
  }

  function changedFilesForItem(item: Item): ChangedFile[] {
    if (!item.payloadMeta) return [];
    try {
      if (item.payloadKind === 'diff' && item.payloadId) {
        const meta = JSON.parse(item.payloadMeta) as DiffMeta;
        return [{
          path: meta.filePath,
          insertions: meta.insertions,
          deletions: meta.deletions,
          kind: meta.changeKind,
          payloadId: item.payloadId,
        }];
      }
      if (item.payloadKind !== 'tool_result' || !item.payloadId) return [];
      const meta = JSON.parse(item.payloadMeta) as ToolResultMeta;
      return (meta.inlineDiff?.files ?? []).map((file) => ({
        path: file.path,
        insertions: file.insertions ?? 0,
        deletions: file.deletions ?? 0,
        kind: file.kind ?? 'modified',
        payloadId: item.payloadId!,
      }));
    } catch (err) {
      console.error('Failed to parse changed-file metadata:', item.id, err);
      return [];
    }
  }

  /**
   * Collect changed files for a given turn by scanning diff-bearing payloads.
   * Returns an array of ChangedFile for use by ChangedFilesTree.
   */
  function getChangedFilesForTurn(turnIndex: number): ChangedFile[] {
    const files: ChangedFile[] = [];
    for (const item of pane.items) {
      if (item.turnIndex !== turnIndex) continue;
      files.push(...changedFilesForItem(item));
    }
    return files;
  }

  /**
   * Build a set of turn indices that have at least one diff item,
   * so we can render ChangedFilesTree at turn boundaries.
   */
  let turnBoundaries = $derived.by((): Map<number, ChangedFile[]> => {
    const turns = new Map<number, ChangedFile[]>();
    for (const item of pane.items) {
      if (!item.payloadId) continue;
      if (item.payloadKind !== 'diff' && item.payloadKind !== 'tool_result') continue;
      if (turns.has(item.turnIndex)) continue;
      const files = getChangedFilesForTurn(item.turnIndex);
      if (files.length > 0) {
        turns.set(item.turnIndex, files);
      }
    }
    return turns;
  });

  /**
   * Aggregate per-turn diff totals keyed by turnIndex, for the inline badge
   * rendered after ChangedFilesTree. Only turns with non-zero line changes
   * produce an entry, so rendering can simply check map presence.
   */
  let turnSummaries = $derived.by((): Map<number, TurnDiffSummary> => {
    const out = new Map<number, TurnDiffSummary>();
    for (const turnIndex of turnBoundaries.keys()) {
      const summary = summarizeTurnDiffs(pane.items, turnIndex);
      if (turnSummaryIsMeaningful(summary)) {
        out.set(turnIndex, summary);
      }
    }
    return out;
  });

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
        {#if item.kind === 'user_text' || item.role === 'user'}
          <UserMessage {item} />
        {:else if item.kind === 'tool_call' || item.kind === 'tool_completion'}
          {#if item.payloadKind === 'proposed_plan' && item.payloadId}
            {@const planMeta = parseMeta<ProposedPlanMeta>(item)}
            {#if planMeta}
              <ProposedPlanCard {pane} payloadId={item.payloadId} meta={planMeta} />
            {:else}
              <ToolResultDropdown {item} />
            {/if}
          {:else if item.payloadKind === 'diff' && item.payloadId}
            {@const diffMeta = parseMeta<DiffMeta>(item)}
            {#if diffMeta}
              <DiffPreview {item} meta={diffMeta} payloadId={item.payloadId} />
            {:else}
              <ToolResultDropdown {item} />
            {/if}
          {:else if item.payloadKind === 'command_output' && item.payloadId}
            {@const cmdMeta = parseMeta<CommandOutputMeta>(item)}
            {#if cmdMeta}
              <CommandOutput {item} meta={cmdMeta} payloadId={item.payloadId} />
            {:else}
              <ToolResultDropdown {item} />
            {/if}
          {:else if item.payloadKind === 'tool_result' && item.payloadId}
            <!-- File-change/command-mutation helpers attached a tool_result
                 payload to the lifecycle row; render the rich diff card so
                 file edits keep their existing visual weight. Gating on
                 payload kind (not just a successful JSON parse) avoids
                 the tool_call_result payload accidentally fitting the
                 ToolResultMeta shape and rendering as an empty card. -->
            {@const toolMeta = parseMeta<ToolResultMeta>(item)}
            {#if toolMeta}
              <ToolResultCard {item} meta={toolMeta} payloadId={item.payloadId} />
            {:else}
              <ToolResultDropdown {item} />
            {/if}
          {:else}
            <ToolResultDropdown {item} />
          {/if}
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
      {@render renderNode(node, 0)}

      {#if isLastRootInTurn(index)}
        {@const turnIndex = rootTurnIndex(node)}
        {@const turnFiles = turnBoundaries.get(turnIndex)}
        {#if turnFiles}
          <ChangedFilesTree files={turnFiles} />
        {/if}
        {@const turnSummary = turnSummaries.get(turnIndex)}
        {#if turnSummary}
          <TurnDiffBadge {pane} {turnIndex} summary={turnSummary} />
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
