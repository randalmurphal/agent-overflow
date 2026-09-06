<script lang="ts">
  // Host CPU% + memory used/total snapshot, pushed from Go every ~2s.
  // Sits directly above SettingsFooter in the sidebar. Hidden until
  // the first event lands so the slot doesn't flash a placeholder.
  import { getSystemStats } from '../../stores/systemStats.svelte';
  import { formatGiB } from '../../utils/format';
  import { selectedBackend } from '../../stores/selectedBackend.svelte';
  import { attachedBackendEntry, backendDisplayName, backendReachable, hasMultipleBackends } from '../../stores/attachedBackends.svelte';

  const backend = $derived(selectedBackend());
  const computer = $derived(attachedBackendEntry(backend));
  const name = $derived(computer ? backendDisplayName(computer) : 'Computer');
  const stats = $derived(backendReachable(backend) ? getSystemStats(backend) : null);
</script>

{#if stats}
  <div
    class="border-t border-border-subtle px-3 py-2 shrink-0 flex flex-col gap-1.5 text-[0.6875rem] leading-tight text-fg-muted"
    data-testid="sidebar-system-stats"
  >
    {#if hasMultipleBackends()}<span class="truncate text-fg-subtle" title={name}>{name}</span>{/if}
    <div class="flex items-center gap-4">
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
  </div>
{/if}
