<script lang="ts">
  import { getSettings } from '../../stores/settings.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ChangedFile, CommandOutputMeta, DiffMeta, Item, ProposedPlanMeta, ToolResultMeta, WorkEntryData } from '../../types/models';
  import AssistantMessage from './AssistantMessage.svelte';
  import ChangedFilesTree from './ChangedFilesTree.svelte';
  import CommandOutput from './CommandOutput.svelte';
  import DiffPreview from './DiffPreview.svelte';
  import ProposedPlanCard from './ProposedPlanCard.svelte';
  import StreamingMessage from './StreamingMessage.svelte';
  import ThinkingBlock from './ThinkingBlock.svelte';
  import ToolResultCard from './ToolResultCard.svelte';
  import UserMessage from './UserMessage.svelte';
  import WorkEntry from './WorkEntry.svelte';

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
   * Check if this item is the last item of its turn, used to decide
   * when to render the ChangedFilesTree summary.
   */
  function isLastItemInTurn(item: Item, index: number): boolean {
    const nextItem = pane.items[index + 1];
    return !nextItem || nextItem.turnIndex !== item.turnIndex;
  }

  // Auto-scroll only when the user is already near the bottom.
  $effect(() => {
    pane.items.length;
    pane.streamingContent;
    pane.activeToolCalls.size;
    pane.pendingMessage;

    if (scrollContainer && userNearBottom) {
      requestAnimationFrame(() => {
        scrollContainer!.scrollTop = scrollContainer!.scrollHeight;
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
    {#each pane.items as item, index (item.id)}
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

      {#if isLastItemInTurn(item, index)}
        {@const turnFiles = turnBoundaries.get(item.turnIndex)}
        {#if turnFiles}
          <ChangedFilesTree files={turnFiles} />
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
      <div class="mb-3 flex flex-col gap-1" role="group" aria-label="Active tool calls">
        {#each activeToolEntries as entry (entry.id)}
          <WorkEntry {entry} />
        {/each}
      </div>
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
