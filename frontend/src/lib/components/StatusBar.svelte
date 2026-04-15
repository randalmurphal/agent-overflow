<script lang="ts">
  import type { ThreadPane } from '../stores/thread.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let statusColor = $derived.by(() => {
    switch (pane.sessionStatus) {
      case 'running': return 'bg-green-400';
      case 'connected':
      case 'ready': return 'bg-accent';
      case 'error': return 'bg-red-400';
      default: return 'bg-text-secondary/50';
    }
  });

  function formatTokens(n: number): string {
    if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M';
    if (n >= 1_000) return (n / 1_000).toFixed(1) + 'k';
    return String(n);
  }

  function formatCost(usd: number): string {
    return '$' + usd.toFixed(4);
  }
</script>

<div class="border-t border-border bg-surface-0 px-4 py-1.5 flex items-center gap-4 text-xs text-text-secondary">
  <div class="flex items-center gap-1.5">
    <span class="w-1.5 h-1.5 rounded-full {statusColor}"></span>
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
    <span class="ml-auto flex items-center gap-3">
      <span>{formatTokens(pane.tokenUsage.inputTokens)} in / {formatTokens(pane.tokenUsage.outputTokens)} out</span>
      {#if pane.tokenUsage.totalCostUsd != null}
        <span>{formatCost(pane.tokenUsage.totalCostUsd)}</span>
      {/if}
    </span>
  {/if}
</div>
