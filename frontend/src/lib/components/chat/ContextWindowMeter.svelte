<script lang="ts">
  import type { ContextWindow } from '../../types/events';
  import { formatTokens } from '../../utils/format';

  let { data }: { data: ContextWindow } = $props();

  let showPopover = $state(false);

  const RADIUS = 9.75;
  const CIRCUMFERENCE = 2 * Math.PI * RADIUS;

  let percentage = $derived(
    data.usedPercentage ?? (data.maxTokens > 0 ? (data.usedTokens / data.maxTokens) * 100 : 0),
  );

  let dashOffset = $derived(CIRCUMFERENCE - (percentage / 100) * CIRCUMFERENCE);

  let strokeColor = $derived(
    percentage > 80 ? 'stroke-amber-400' : 'stroke-text-secondary',
  );

  let displayPct = $derived(Math.round(percentage));
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="relative inline-flex items-center"
  onmouseenter={() => showPopover = true}
  onmouseleave={() => showPopover = false}
>
  <svg class="w-6 h-6 -rotate-90" viewBox="0 0 24 24">
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
  <span class="absolute inset-0 flex items-center justify-center text-[7px] font-medium text-text-secondary rotate-0">
    {displayPct}
  </span>

  {#if showPopover}
    <div class="absolute bottom-full left-1/2 -translate-x-1/2 mb-2 z-50 bg-surface-1 border border-border rounded-md shadow-lg px-3 py-2 min-w-[160px]">
      <p class="text-[10px] font-semibold text-text-secondary uppercase tracking-wider mb-1.5">Context window</p>
      <div class="space-y-0.5 text-xs text-text-secondary">
        <p>{displayPct}% used</p>
        <p>{formatTokens(data.usedTokens)}{data.maxTokens > 0 ? ` / ${formatTokens(data.maxTokens)}` : ''} tokens</p>
        {#if data.totalProcessed > data.usedTokens}
          <p class="text-text-secondary/60">Total processed: {formatTokens(data.totalProcessed)}</p>
        {/if}
      </div>
    </div>
  {/if}
</div>
