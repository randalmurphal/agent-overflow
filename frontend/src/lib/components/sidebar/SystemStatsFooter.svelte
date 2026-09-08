<script lang="ts">
  import { getSystemStats } from '../../stores/systemStats.svelte';
  import { formatGiB } from '../../utils/format';
  import { hasMultipleBackends } from '../../stores/attachedBackends.svelte';
  import { selectedTelemetryComputers } from '../../stores/telemetryComputers.svelte';
  import TelemetryComputerPicker from '../usage/TelemetryComputerPicker.svelte';

  const computers = $derived(selectedTelemetryComputers('system'));
  const rows = $derived(computers.map((computer) => ({ ...computer, stats: computer.connected ? getSystemStats(computer.key) : null })));
  const multiple = $derived(hasMultipleBackends());
</script>

{#if multiple || rows.some((row) => row.stats)}
  <div class="border-t border-border-subtle px-3 py-2 shrink-0 flex flex-col gap-2 text-[0.6875rem] leading-tight text-fg-muted" data-testid="sidebar-system-stats">
    {#if multiple}<TelemetryComputerPicker kind="system" />{/if}
    {#each rows as row (row.key)}
      <div class="flex min-w-0 flex-col gap-1" data-testid="system-stats-computer">
        {#if rows.length > 1}
          <span class="truncate text-fg-subtle" title={row.name}>{row.name}{row.stats?.isWsl ? ' · WSL' : ''}</span>
        {:else if row.stats?.isWsl}<span class="text-fg-subtle">WSL</span>{/if}
        {#if row.stats}
          <div class="flex min-w-0 items-center justify-between gap-2 tabular-nums whitespace-nowrap">
            <span><span class="text-fg-subtle">CPU</span> {Math.round(row.stats.cpuPercent)}%</span>
            <span><span class="text-fg-subtle">RAM</span> {formatGiB(row.stats.memUsedBytes)} / {formatGiB(row.stats.memTotalBytes)} GB</span>
          </div>
        {:else}<span class="text-fg-subtle">{row.connected ? 'Waiting for system stats…' : 'Offline'}</span>{/if}
      </div>
    {/each}
    {#if rows.length === 0}<span class="text-fg-subtle">Selected computer is unavailable.</span>{/if}
  </div>
{/if}
