<script lang="ts">
  import { onDestroy } from 'svelte';
  import type { ContextWindow } from '../../types/events';
  import type { Thread } from '../../types/models';
  import { formatTokens } from '../../utils/format';
  import SlidersHorizontal from 'lucide-svelte/icons/sliders-horizontal';
  import Icon from '../primitives/Icon.svelte';
  import Popover from '../primitives/Popover.svelte';

  let { data, thread }: { data: ContextWindow; thread?: Thread | null } = $props();

  let buttonEl: HTMLButtonElement | undefined = $state(undefined);
  let showPopover = $state(false);
  let closeTimer: number | null = null;

  const RADIUS = 9.75;
  const CIRCUMFERENCE = 2 * Math.PI * RADIUS;

  let maxTokens = $derived(data.maxTokens ?? 0);
  let availableTokens = $derived(data.autoCompactTokenLimit ?? maxTokens);
  let percentage = $derived(
    availableTokens > 0 ? (data.usedTokens / availableTokens) * 100 : (data.usedPercentage ?? 0),
  );

  let dashOffset = $derived(CIRCUMFERENCE - (percentage / 100) * CIRCUMFERENCE);

  let strokeColor = $derived(
    percentage > 95 ? 'stroke-error' : percentage > 80 ? 'stroke-warning' : 'stroke-fg-subtle',
  );

  let displayPct = $derived(Math.round(percentage));

  function openPopover(): void {
    if (closeTimer !== null) {
      window.clearTimeout(closeTimer);
      closeTimer = null;
    }
    showPopover = true;
  }

  function scheduleClose(): void {
    if (closeTimer !== null) window.clearTimeout(closeTimer);
    closeTimer = window.setTimeout(() => {
      showPopover = false;
      closeTimer = null;
    }, 140);
  }

  function openContextSettings(): void {
    if (!thread) return;
    window.dispatchEvent(new CustomEvent('agent-overflow:open-settings', {
      detail: {
        section: 'providers',
        contextTarget: {
          threadId: thread.id,
          provider: thread.provider,
          model: thread.model,
          contextWindow: thread.contextWindow,
          autoCompactStandardPercent: thread.autoCompactStandardPercent ?? 0,
          autoCompactExtendedPercent: thread.autoCompactExtendedPercent ?? 0,
        },
      },
    }));
    showPopover = false;
  }

  onDestroy(() => {
    if (closeTimer !== null) window.clearTimeout(closeTimer);
  });
</script>

<button
  bind:this={buttonEl}
  type="button"
  class="relative inline-flex h-7 w-7 items-center justify-center bg-transparent border-none p-0 cursor-help focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded-full hover:bg-surface-2/30 transition-colors"
  aria-label="Context Window: {displayPct}% used"
  onmouseenter={openPopover}
  onmouseleave={scheduleClose}
  onfocus={openPopover}
  onblur={scheduleClose}
>
  <svg class="w-7 h-7 -rotate-90" viewBox="0 0 24 24" aria-hidden="true">
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
  <span class="absolute inset-0 grid place-items-center text-[8px] leading-none font-semibold tabular-nums text-text-secondary rotate-0 translate-y-px" aria-hidden="true">
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
      onmouseenter={openPopover}
      onmouseleave={scheduleClose}
      class="relative bg-surface-1 border border-border-subtle rounded-[var(--radius-control)] shadow-menu px-3 py-2 min-w-[190px]"
    >
      {#if thread}
        <button
          type="button"
          onclick={openContextSettings}
          title="Context settings"
          aria-label="Context settings"
          class="absolute right-2 top-2 inline-flex h-6 w-6 items-center justify-center rounded-[var(--radius-field)] text-fg-hint hover:text-fg hover:bg-surface-2/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors cursor-pointer"
        >
          <Icon icon={SlidersHorizontal} size={13} strokeWidth={1.8} />
        </button>
      {/if}
      <p class="mb-1.5 pr-7 text-[10px] font-semibold text-fg-subtle uppercase tracking-wider">Context window</p>
      <div class="space-y-0.5 text-xs text-fg-muted">
        <p>{displayPct}% used</p>
        <p>{formatTokens(data.usedTokens)}{availableTokens > 0 ? ` / ${formatTokens(availableTokens)}` : ''} tokens</p>
        {#if data.autoCompactPercent}
          <p class="text-fg-hint">
            Compact at {data.autoCompactPercent}%{data.autoCompactTokenLimit ? ` (${formatTokens(data.autoCompactTokenLimit)})` : ''}
          </p>
        {/if}
      </div>
    </div>
  {/snippet}
</Popover>
