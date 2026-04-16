<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { formatTokens, formatCost } from '../../utils/format';

  let { pane }: { pane: ThreadPane } = $props();

  let statusColor = $derived.by(() => {
    switch (pane.sessionStatus) {
      case 'running': return 'bg-success';
      case 'connected':
      case 'ready': return 'bg-accent';
      case 'error': return 'bg-error';
      default: return 'bg-text-secondary/50';
    }
  });
</script>

<div class="border-t border-border bg-surface-0 px-4 py-1.5 flex items-center gap-4 text-xs text-text-secondary">
  <div class="flex items-center gap-1.5" role="status" aria-live="polite">
    <span class="w-1.5 h-1.5 rounded-full {statusColor}" aria-hidden="true"></span>
    <span>{pane.sessionStatus}</span>
  </div>

  {#if pane.thread}
    <span class="text-text-secondary/50">|</span>
    <span>{pane.thread.provider}</span>
    {#if pane.thread.model}
      <span class="text-text-secondary/50">|</span>
      <span>{pane.thread.model}</span>
    {/if}
  {/if}

  {#if pane.tokenUsage}
    <span class="ml-auto flex items-center gap-3 min-w-0 shrink truncate">
      <span>{formatTokens(pane.tokenUsage.inputTokens)} in / {formatTokens(pane.tokenUsage.outputTokens)} out</span>
      {#if pane.tokenUsage.totalCostUsd != null}
        <span>{formatCost(pane.tokenUsage.totalCostUsd)}</span>
      {/if}
    </span>
  {/if}
</div>
