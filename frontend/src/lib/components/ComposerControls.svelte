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

  let providerLabel = $derived(
    pane.thread?.provider === 'claude' ? 'Claude'
      : pane.thread?.provider === 'codex' ? 'Codex'
      : pane.thread?.provider ?? ''
  );
</script>

{#if pane.thread}
  <div class="px-4 py-1 flex items-center gap-3 text-xs text-text-secondary">
    <span class="font-medium">{providerLabel}</span>

    {#if pane.thread.model}
      <span class="text-text-secondary/50">|</span>
      <span>{pane.thread.model}</span>
    {/if}

    <span class="text-text-secondary/50">|</span>
    <span class="flex items-center gap-1.5">
      <span class="w-1.5 h-1.5 rounded-full {statusColor}"></span>
      <span>{pane.sessionStatus}</span>
    </span>
  </div>
{/if}
