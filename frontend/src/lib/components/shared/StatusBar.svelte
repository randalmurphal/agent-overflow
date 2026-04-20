<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { formatTokens } from '../../utils/format';

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
      default: return 'bg-text-secondary/50';
    }
  });
</script>

<div class="border-t border-border bg-surface-0 px-4 py-1.5 flex items-center gap-4 text-xs text-text-secondary">
  <div class="flex items-center gap-1.5" role="status" aria-live="polite">
    <span class="w-1.5 h-1.5 rounded-full {statusColor}" aria-hidden="true"></span>
    <span>{statusLabel}</span>
  </div>

  {#if pane.thread}
    <span class="text-text-secondary/50" aria-hidden="true">|</span>
    <span>{pane.thread.provider}</span>
    {#if pane.thread.model}
      <span class="text-text-secondary/50" aria-hidden="true">|</span>
      <span>{pane.thread.model}</span>
    {/if}
  {/if}

  {#if pane.contextWindow}
    <span class="ml-auto flex items-center gap-3 min-w-0 shrink truncate">
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
