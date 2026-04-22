<script lang="ts">
  // Donut meter + hover-revealed popover with token breakdown. The
  // popover routes through the Popover primitive so it portals to body
  // and won't be clipped by any ancestor with `overflow-hidden` /
  // `backdrop-filter`. Hover + focus still drive `showPopover` locally;
  // Popover.onClose fires on Escape or outside mousedown for AT users.

  import type { ContextWindow } from '../../types/events';
  import { formatTokens } from '../../utils/format';
  import Popover from '../primitives/Popover.svelte';

  let { data }: { data: ContextWindow } = $props();

  let buttonEl: HTMLButtonElement | undefined = $state(undefined);
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
    percentage > 95 ? 'stroke-error' : percentage > 80 ? 'stroke-warning' : 'stroke-fg-subtle',
  );

  let displayPct = $derived(Math.round(percentage));
</script>

<button
  bind:this={buttonEl}
  type="button"
  class="relative inline-flex items-center bg-transparent border-none p-0 cursor-help focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded-full"
  aria-label="Context window: {displayPct}% used"
  onmouseenter={() => (showPopover = true)}
  onmouseleave={() => (showPopover = false)}
  onfocus={() => (showPopover = true)}
  onblur={() => (showPopover = false)}
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
</button>

<Popover
  anchor={buttonEl}
  open={showPopover}
  onClose={() => (showPopover = false)}
  placement="top-end"
  role="none"
>
  {#snippet children()}
    <div
      role="tooltip"
      class="bg-surface-1 border border-border-subtle rounded-[var(--radius-control)] shadow-menu px-3 py-2 min-w-[160px]"
    >
      <p class="text-[10px] font-semibold text-fg-subtle uppercase tracking-wider mb-1.5">Context window</p>
      <div class="space-y-0.5 text-xs text-fg-muted">
        <p>{displayPct}% used</p>
        <p>{formatTokens(data.usedTokens)}{maxTokens > 0 ? ` / ${formatTokens(maxTokens)}` : ''} tokens</p>
        {#if totalProcessed > data.usedTokens}
          <p class="text-fg-hint">Total processed: {formatTokens(totalProcessed)}</p>
        {/if}
      </div>
    </div>
  {/snippet}
</Popover>
