<script lang="ts">
  // Host CPU% + memory used/total snapshot, pushed from Go every ~2s.
  // Sits directly above SettingsFooter in the sidebar. Hidden until
  // the first event lands so the slot doesn't flash a placeholder.
  import { getSystemStats } from '../../stores/systemStats.svelte';
  import { formatGiB } from '../../utils/format';

  const stats = $derived(getSystemStats());
</script>

{#if stats}
  <div
    class="border-t border-border-subtle px-3 py-2 shrink-0 flex items-center gap-4 text-[11px] leading-tight text-fg-muted"
    data-testid="sidebar-system-stats"
  >
    {#if stats.isWsl}
      <span class="text-fg-subtle uppercase tracking-[0.12em]">WSL</span>
    {/if}
    <span class="inline-flex items-center gap-1.5 tabular-nums">
      <span class="text-fg-subtle uppercase tracking-[0.12em]">CPU</span>
      {Math.round(stats.cpuPercent)}%
    </span>
    <span class="inline-flex items-center gap-1.5 tabular-nums">
      <span class="text-fg-subtle uppercase tracking-[0.12em]">MEM</span>
      {formatGiB(stats.memUsedBytes)} / {formatGiB(stats.memTotalBytes)} GB
    </span>
  </div>
{/if}
