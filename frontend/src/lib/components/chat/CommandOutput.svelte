<script lang="ts">
  import type { CommandOutputMeta } from '../../types/models';
  import { GetPayloadData } from '../../stores/bindings';

  let { meta, payloadId }: { meta: CommandOutputMeta; payloadId: string } = $props();

  let expanded = $state(false);
  let loading = $state(false);
  let fullOutput = $state<string | null>(null);
  let loadError = $state<string | null>(null);

  async function toggle() {
    if (expanded) {
      expanded = false;
      return;
    }

    expanded = true;

    if (fullOutput !== null) return;

    loading = true;
    loadError = null;
    try {
      fullOutput = await GetPayloadData(payloadId);
    } catch (err) {
      loadError = err instanceof Error ? err.message : String(err);
    } finally {
      loading = false;
    }
  }

  let displayText = $derived(expanded && fullOutput !== null ? fullOutput : meta.preview);

  let exitBadgeClasses = $derived(
    meta.exitCode === 0
      ? 'bg-green-700/50 text-green-300'
      : 'bg-red-700/50 text-red-300'
  );
</script>

<div class="bg-surface-1 rounded border border-border overflow-hidden mb-2">
  <!-- Header -->
  <button
    class="w-full px-3 py-2 flex items-center gap-2 text-sm cursor-pointer hover:bg-surface-2/40"
    onclick={toggle}
  >
    <span class="text-xs text-text-secondary select-none">{expanded ? '▼' : '▶'}</span>
    <span class="font-mono text-xs text-text-primary truncate">{meta.command}</span>
    <span class="px-1.5 py-0.5 rounded-full text-xs {exitBadgeClasses}">
      exit {meta.exitCode}
    </span>
    <span class="ml-auto text-xs text-text-secondary shrink-0">
      {meta.lineCount} lines
    </span>
  </button>

  <!-- Output content -->
  {#if displayText}
    <div class="border-t border-border bg-surface-0 px-3 py-2 overflow-x-auto">
      {#if loading}
        <p class="text-xs text-text-secondary">Loading full output…</p>
      {:else if loadError}
        <p class="text-xs text-red-400">Failed to load output: {loadError}</p>
      {/if}

      <pre class="font-mono text-xs whitespace-pre text-text-secondary">{displayText}</pre>
    </div>
  {/if}
</div>
