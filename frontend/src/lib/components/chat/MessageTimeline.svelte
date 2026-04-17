<script lang="ts">
  import { getSettings } from '../../stores/settings.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ChangedFile, CommandOutputMeta, DiffMeta, Item, ProposedPlanMeta, ToolResultMeta, WorkEntryData } from '../../types/models';
  import { groupItemsBySubagent, type TimelineNode } from '../../utils/subagentGrouping';
  import { summarizeTurnDiffs, turnSummaryIsMeaningful, type TurnDiffSummary } from '../../utils/turnDiffSummary';
  import ActiveToolsGroup from './ActiveToolsGroup.svelte';
  import AssistantMessage from './AssistantMessage.svelte';
  import ChangedFilesTree from './ChangedFilesTree.svelte';
  import CommandOutput from './CommandOutput.svelte';
  import DiffPreview from './DiffPreview.svelte';
  import ProposedPlanCard from './ProposedPlanCard.svelte';
  import StreamingMessage from './StreamingMessage.svelte';
  import SubagentGroup from './SubagentGroup.svelte';
  import ThinkingBlock from './ThinkingBlock.svelte';
  import ToolResultCard from './ToolResultCard.svelte';
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

  // Active tool calls as WorkEntryData for rendering.
  let activeToolEntries = $derived<WorkEntryData[]>(
    [...pane.activeToolCalls.entries()].map(([id, data]) => {
      const meta = data as Record<string, unknown> | null;
      return {
        id,
        type: (meta && typeof meta.toolName === 'string') ? meta.toolName : 'tool',
        name: (meta && typeof meta.toolName === 'string') ? meta.toolName : undefined,
        status: 'running' as const,
        meta: data,
      };
    })
  );

  function parseMeta<T>(payloadId: string | undefined): T | null {
    if (!payloadId) return null;
    const pm = pane.payloadMetas.get(payloadId);
    if (!pm) return null;
    try {
      return JSON.parse(pm.meta) as T;
    } catch (err) {
      console.error('Failed to parse payload meta:', payloadId, err);
      return null;
    }
  }

  /**
   * Collect changed files for a given turn by scanning diff items.
   * Returns an array of ChangedFile for use by ChangedFilesTree.
   */
  function getChangedFilesForTurn(turnIndex: number): ChangedFile[] {
    const files: ChangedFile[] = [];
    for (const item of pane.items) {
      if (item.turnIndex !== turnIndex) continue;
      if (item.kind !== 'diff' || !item.payloadId) continue;
      const meta = parseMeta<DiffMeta>(item.payloadId);
      if (!meta) continue;
      files.push({
        path: meta.filePath,
        insertions: meta.insertions,
        deletions: meta.deletions,
        kind: meta.changeKind,
        payloadId: item.payloadId,
      });
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
      if (item.kind !== 'diff' || !item.payloadId) continue;
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
      const summary = summarizeTurnDiffs(pane.items, turnIndex, (id) => parseMeta<DiffMeta>(id));
      if (turnSummaryIsMeaningful(summary)) {
        out.set(turnIndex, summary);
      }
    }
    return out;
  });

  /**
   * Build the subagent-aware render tree. Items are grouped into subagent
   * cards when they declare a parentToolUseId matching a parent tool item;
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
    pane.streamingContent;
    pane.activeToolCalls.size;
    pane.pendingMessage;

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
      {#if item.role === 'user'}
        <UserMessage {item} />
      {:else if item.kind === 'diff' && item.payloadId}
        {@const diffMeta = parseMeta<DiffMeta>(item.payloadId)}
        {#if diffMeta}
          <DiffPreview meta={diffMeta} payloadId={item.payloadId} />
        {:else}
          <AssistantMessage {item} />
        {/if}
      {:else if item.kind === 'command_execution' && item.payloadId}
        {@const cmdMeta = parseMeta<CommandOutputMeta>(item.payloadId)}
        {#if cmdMeta}
          <CommandOutput meta={cmdMeta} payloadId={item.payloadId} />
        {:else}
          <AssistantMessage {item} />
        {/if}
      {:else if item.kind === 'tool_result' && item.payloadId}
        {@const toolMeta = parseMeta<ToolResultMeta>(item.payloadId)}
        {#if toolMeta}
          <ToolResultCard {item} meta={toolMeta} payloadId={item.payloadId} />
        {:else}
          <AssistantMessage {item} />
        {/if}
      {:else if item.kind === 'proposed_plan' && item.payloadId}
        {@const planMeta = parseMeta<ProposedPlanMeta>(item.payloadId)}
        {#if planMeta}
          <ProposedPlanCard {pane} payloadId={item.payloadId} meta={planMeta} />
        {:else}
          <AssistantMessage {item} />
        {/if}
      {:else if item.kind === 'thinking'}
        <ThinkingBlock {item} />
      {:else}
        <AssistantMessage {item} />
      {/if}
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

    {#if pane.pendingMessage}
      <div class="flex justify-end mb-3">
        <div class="max-w-[85%] rounded-lg px-4 py-2.5 bg-accent/20 text-text-primary opacity-60">
          <p class="whitespace-pre-wrap text-sm leading-relaxed">{pane.pendingMessage}</p>
        </div>
      </div>
    {/if}

    {#if activeToolEntries.length > 0}
      <ActiveToolsGroup entries={activeToolEntries} />
    {/if}

    {#if pane.streamingContent}
      {#if getSettings().streamingEnabled}
        <StreamingMessage content={pane.streamingContent} />
      {:else}
        <div class="flex justify-start mb-3" role="status" aria-live="polite">
          <div class="max-w-[85%] rounded-lg px-4 py-2.5 bg-surface-2 text-text-secondary text-sm">
            <span class="animate-pulse">Thinking...</span>
          </div>
        </div>
      {/if}
    {/if}

    {#if pane.items.length === 0 && !pane.streamingContent && activeToolEntries.length === 0 && !pane.pendingMessage}
      <div class="flex items-center justify-center h-full text-text-secondary text-sm">
        No messages yet. Send a message to get started.
      </div>
    {/if}
  {/if}
</div>
