<script lang="ts">
  import { fade } from 'svelte/transition';
  import type { ContextWindow } from '../../types/events';
  import { formatTokens } from '../../utils/format';

  let { data }: { data: ContextWindow } = $props();

  let showPopover = $state(false);

  const RADIUS = 9.75;
  const CIRCUMFERENCE = 2 * Math.PI * RADIUS;

  let maxTokens = $derived(data.maxTokens ?? 0);
  let totalProcessed = $derived(data.totalProcessed ?? 0);

  let percentage = $derived(
    data.usedPercentage ?? (maxTokens > 0 ? (data.usedTokens / maxTokens) * 100 : 0),
  );

  let dashOffset = $derived(CIRCUMFERENCE - (percentage / 100) * CIRCUMFERENCE);

  let strokeColor = $derived(
    percentage > 95 ? 'stroke-error' : percentage > 80 ? 'stroke-warning' : 'stroke-text-secondary',
  );

  let displayPct = $derived(Math.round(percentage));
</script>

<button
  type="button"
  class="relative inline-flex items-center bg-transparent border-none p-0 cursor-help focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded-full"
  aria-label="Context window: {displayPct}% used"
  onmouseenter={() => showPopover = true}
  onmouseleave={() => showPopover = false}
  onfocus={() => showPopover = true}
  onblur={() => showPopover = false}
>
  <svg class="w-6 h-6 -rotate-90" viewBox="0 0 24 24" aria-hidden="true">
    <circle
      cx="12" cy="12" r={RADIUS}
      fill="none"
      stroke-width="3"
      class="stroke-text-secondary/20"
    />
    <circle
      cx="12" cy="12" r={RADIUS}
      fill="none"
      stroke-width="3"
      stroke-linecap="round"
      stroke-dasharray={CIRCUMFERENCE}
      stroke-dashoffset={dashOffset}
      class={strokeColor}
    />
  </svg>
  <span class="absolute inset-0 flex items-center justify-center text-[7px] font-medium text-text-secondary rotate-0" aria-hidden="true">
    {displayPct}
  </span>

  {#if showPopover}
    <div role="tooltip" transition:fade={{ duration: 100 }} class="absolute bottom-full left-1/2 -translate-x-1/2 mb-2 z-30 bg-surface-1 border border-border rounded-md shadow-lg px-3 py-2 min-w-[160px]">
      <p class="text-[10px] font-semibold text-text-secondary uppercase tracking-wider mb-1.5">Context window</p>
      <div class="space-y-0.5 text-xs text-text-secondary">
        <p>{displayPct}% used</p>
        <p>{formatTokens(data.usedTokens)}{maxTokens > 0 ? ` / ${formatTokens(maxTokens)}` : ''} tokens</p>
        {#if totalProcessed > data.usedTokens}
          <p class="text-text-secondary/60">Total processed: {formatTokens(totalProcessed)}</p>
        {/if}
      </div>
    </div>
  {/if}
</button>
