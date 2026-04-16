<script lang="ts">
  import type { RateLimitEntry } from '../../types/events';

  let { limits }: { limits: RateLimitEntry[] } = $props();

  function barColor(pct: number): string {
    if (pct > 95) return 'bg-error';
    if (pct > 80) return 'bg-warning';
    return 'bg-accent';
  }

  function textColor(pct: number): string {
    if (pct > 95) return 'text-error';
    if (pct > 80) return 'text-warning';
    return 'text-text-secondary';
  }

  function formatWindow(mins: number): string {
    if (mins >= 1440) {
      const days = Math.round(mins / 1440);
      return `${days}d`;
    }
    if (mins >= 60) {
      const hours = Math.round(mins / 60);
      return `${hours}h`;
    }
    return `${mins}m`;
  }
</script>

{#if limits.length > 0}
  <div class="flex items-center gap-2">
    {#each limits as entry}
      <div class="flex items-center gap-1.5" title="{entry.limitName}: {Math.round(entry.usedPercent)}% ({formatWindow(entry.windowMins)})">
        <span class="text-[10px] {textColor(entry.usedPercent)} tabular-nums">{Math.round(entry.usedPercent)}%</span>
        <div class="w-8 h-1.5 rounded-full bg-surface-2 overflow-hidden">
          <div
            class="h-full rounded-full transition-all {barColor(entry.usedPercent)}"
            style="width: {Math.min(entry.usedPercent, 100)}%"
          ></div>
        </div>
        <span class="text-[9px] text-text-secondary/50">{formatWindow(entry.windowMins)}</span>
      </div>
    {/each}
  </div>
{/if}
