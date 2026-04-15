<script lang="ts">
  import type { ThreadPane } from '../stores/thread.svelte';
  import type { DiffMeta, CommandOutputMeta } from '../types/models';
  import UserMessage from './UserMessage.svelte';
  import AssistantMessage from './AssistantMessage.svelte';
  import DiffPreview from './DiffPreview.svelte';
  import CommandOutput from './CommandOutput.svelte';
  import WorkEntry, { type WorkEntryData } from './WorkEntry.svelte';

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
    } catch {
      return null;
    }
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

<div bind:this={scrollContainer} onscroll={handleScroll} class="flex-1 overflow-y-auto px-4 py-4">
  {#if pane.loading}
    <div class="flex items-center justify-center h-full text-text-secondary text-sm">
      <span class="animate-pulse">Loading thread...</span>
    </div>
  {:else}
    {#each pane.items as item (item.id)}
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
      {:else if item.kind === 'thinking'}
        <div class="mb-2 px-3 py-2 bg-surface-1 rounded border border-border text-xs text-text-secondary italic">
          Thinking: {item.summary}
        </div>
      {:else}
        <AssistantMessage {item} />
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
      <div class="mb-3 flex flex-col gap-1">
        {#each activeToolEntries as entry (entry.id)}
          <WorkEntry {entry} />
        {/each}
      </div>
    {/if}

    {#if pane.streamingContent}
      <div class="flex justify-start mb-3">
        <div class="max-w-[85%] rounded-lg px-4 py-2.5 bg-surface-2 text-text-primary">
          <p class="whitespace-pre-wrap text-sm leading-relaxed">{pane.streamingContent}</p>
          <span class="inline-block w-1.5 h-4 bg-accent animate-pulse ml-0.5 align-text-bottom"></span>
        </div>
      </div>
    {/if}

    {#if pane.items.length === 0 && !pane.streamingContent && activeToolEntries.length === 0 && !pane.pendingMessage}
      <div class="flex items-center justify-center h-full text-text-secondary text-sm">
        No messages yet. Send a message to get started.
      </div>
    {/if}
  {/if}
</div>
