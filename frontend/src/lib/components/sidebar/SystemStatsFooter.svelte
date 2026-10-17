<script lang="ts">
  import { getSystemStats } from '../../stores/systemStats.svelte';
  import { formatGiB } from '../../utils/format';
  import { hasMultipleBackends } from '../../stores/attachedBackends.svelte';
  import { selectedTelemetryComputers, telemetrySelection } from '../../stores/telemetryComputers.svelte';
  import { getTelemetryFooterShape, recordTelemetryFooterShape, telemetryFooterShapeKey, type FooterShape } from '../../stores/telemetryFooterShape.svelte';
  import TelemetryComputerPicker from '../usage/TelemetryComputerPicker.svelte';

  const computers = $derived(selectedTelemetryComputers('system'));
  const rows = $derived(computers.map((computer) => ({ ...computer, stats: computer.connected ? getSystemStats(computer.key) : null })));
  const multiple = $derived(hasMultipleBackends());

  // Settled once every selected computer has a sample or is offline. The
  // attached list is published by the manifest before the socket that
  // carries samples opens, so a settled footer has its full list.
  const settled = $derived((rows.length > 0 || multiple) && rows.every((row) => row.stats !== null || !(row.connected || row.starting)));
  const shapeKey = $derived(telemetryFooterShapeKey('system', telemetrySelection('system')));
  // The one shape both the paint and the record read: until settled, at
  // least the last settled shape for this selection, so the footer does
  // not grow when the attached list or the first sample arrives.
  const shape: FooterShape = $derived.by(() => {
    if (settled) return { lines: rows.length, picker: multiple };
    const recorded = getTelemetryFooterShape(shapeKey);
    return { lines: Math.max(rows.length, recorded?.lines ?? 0), picker: multiple || (recorded?.picker ?? false) };
  });
  $effect(() => {
    if (settled) recordTelemetryFooterShape(shapeKey, shape);
  });
  const slots = $derived(Array.from({ length: shape.lines }, (_, i) => rows[i] ?? null));
</script>

{#if shape.picker || shape.lines > 0}
  <div class="border-t border-border-subtle px-3 py-2 shrink-0 flex flex-col gap-2 text-[0.6875rem] leading-tight text-fg-muted" data-testid="sidebar-system-stats">
    {#if shape.picker}<TelemetryComputerPicker kind="system" />{/if}
    {#each slots as row, i (row?.key ?? `placeholder-${i}`)}
      <div class="flex min-w-0 flex-col gap-1" data-testid={row ? 'system-stats-computer' : 'system-stats-placeholder'}>
        {#if shape.lines > 1}
          <span class="truncate text-fg-subtle" title={row?.name}>{row?.name ?? ' '}</span>
        {/if}
        {#if row?.stats}
          <div class="flex min-w-0 items-center justify-between gap-2 tabular-nums whitespace-nowrap">
            {#if row.stats.isWsl}<span class="text-fg-subtle">WSL</span>{/if}
            <span><span class="text-fg-subtle">CPU</span> {Math.round(row.stats.cpuPercent)}%</span>
            <span><span class="text-fg-subtle">RAM</span> {formatGiB(row.stats.memUsedBytes)} / {formatGiB(row.stats.memTotalBytes)} GB</span>
          </div>
        {:else}<span class="text-fg-subtle">{!row || row.connected || row.starting ? 'Waiting for system stats…' : 'Offline'}</span>{/if}
      </div>
    {/each}
    {#if shape.lines === 0}<span class="text-fg-subtle">Selected computer is unavailable.</span>{/if}
  </div>
{/if}
