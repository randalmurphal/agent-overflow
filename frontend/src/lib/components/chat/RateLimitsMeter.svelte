<script lang="ts">
  import type { RateLimitEntry } from '../../types/events';

  let { limits }: { limits: RateLimitEntry[] } = $props();

  function barColor(pct: number): string {
    if (pct > 95) return 'bg-red-500';
    if (pct > 80) return 'bg-amber-400';
    return 'bg-accent';
  }

  function textColor(pct: number): string {
    if (pct > 95) return 'text-red-400';
    if (pct > 80) return 'text-amber-400';
    return 'text-text-secondary';
  }
</script>

{#if limits.length > 0}
  <div class="flex items-center gap-2">
    {#each limits as entry}
      <div class="flex items-center gap-1.5" title="{entry.name}: {entry.used}/{entry.limit} ({entry.window})">
        <span class="text-[10px] {textColor(entry.percentage)} tabular-nums">{Math.round(entry.percentage)}%</span>
        <div class="w-8 h-1.5 rounded-full bg-surface-2 overflow-hidden">
          <div
            class="h-full rounded-full transition-all {barColor(entry.percentage)}"
            style="width: {Math.min(entry.percentage, 100)}%"
          ></div>
        </div>
        <span class="text-[9px] text-text-secondary/50">{entry.window}</span>
      </div>
    {/each}
  </div>
{/if}
