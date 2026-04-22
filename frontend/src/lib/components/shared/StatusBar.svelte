<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { formatTokens } from '../../utils/format';
  import Separator from '../primitives/Separator.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let statusLabel = $derived.by(() => {
    if (pane.generalError) return 'error';
    if (pane.isTurnActive) return 'running';
    return 'ready';
  });

  let statusColor = $derived.by(() => {
    switch (statusLabel) {
      case 'running': return 'bg-success';
      case 'ready': return 'bg-accent';
      case 'error': return 'bg-error';
      default: return 'bg-fg-subtle';
    }
  });
</script>

<div class="border-t border-border-subtle px-5 py-1 flex items-center gap-3 text-[10px] text-fg-subtle">
  <div class="flex items-center gap-1.5" role="status" aria-live="polite">
    <span class="w-1.5 h-1.5 rounded-full {statusColor}" aria-hidden="true"></span>
    <span>{statusLabel}</span>
  </div>

  {#if pane.thread}
    <Separator orientation="vertical" opacity={0.5} class="h-3" />
    <span>{pane.thread.provider}</span>
    {#if pane.thread.model}
      <Separator orientation="vertical" opacity={0.5} class="h-3" />
      <span>{pane.thread.model}</span>
    {/if}
  {/if}

  {#if pane.contextWindow}
    <span class="ml-auto flex items-center gap-2 min-w-0 shrink truncate tabular-nums">
      <span class="truncate">
        {formatTokens(pane.contextWindow.usedTokens)}
        {#if pane.contextWindow.maxTokens}
          / {formatTokens(pane.contextWindow.maxTokens)}
        {/if}
      </span>
      {#if pane.contextWindow.usedPercentage != null}
        <span class="shrink-0">{Math.round(pane.contextWindow.usedPercentage)}%</span>
      {/if}
    </span>
  {/if}
</div>
