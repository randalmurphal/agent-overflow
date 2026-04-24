<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Item } from '../../types/models';
  import AssistantMessage from './AssistantMessage.svelte';
  import NotificationRow from './NotificationRow.svelte';
  import TerminalInteractionRow from './TerminalInteractionRow.svelte';
  import ThinkingBlock from './ThinkingBlock.svelte';
  import ToolCallCard from './ToolCallCard.svelte';
  import UserMessage from './UserMessage.svelte';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';

  let {
    pane,
    item,
    orphan = false,
    onImageExpand,
  }: {
    pane: ThreadPane;
    item: Item;
    orphan?: boolean;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
  } = $props();

  const displayItem = $derived.by(() => {
    const liveSummary = pane.liveItemSummaries[item.id];
    if (liveSummary === undefined) {
      return item;
    }
    return { ...item, summary: liveSummary };
  });
</script>

<div data-item-id={displayItem.id}>
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
  {#if displayItem.kind === 'user_text'}
    <UserMessage item={displayItem} {onImageExpand} />
  {:else if displayItem.kind === 'tool_call' || displayItem.kind === 'tool_completion'}
    <ToolCallCard {pane} item={displayItem} />
  {:else if displayItem.kind === 'thinking'}
    <ThinkingBlock item={displayItem} />
  {:else if displayItem.kind === 'terminal_interaction'}
    <TerminalInteractionRow item={displayItem} />
  {:else if displayItem.kind === 'notification'}
    <NotificationRow item={displayItem} />
  {:else if displayItem.kind === 'error'}
    <div class="mb-4 rounded-[var(--radius-control)] border border-error/30 bg-error/10 px-3 py-2 text-sm text-error">
      {displayItem.summary}
    </div>
  {:else if displayItem.kind === 'compaction'}
    <div class="mb-4 flex items-center gap-3 text-[10px] uppercase tracking-[0.18em] text-fg-subtle">
      <div class="h-px flex-1 bg-border-subtle"></div>
      <span>{displayItem.summary || 'Context compacted'}</span>
      <div class="h-px flex-1 bg-border-subtle"></div>
    </div>
  {:else}
    <AssistantMessage item={displayItem} />
  {/if}
</div>
