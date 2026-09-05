<script lang="ts">
  import type { ContextWindow } from '../../types/events';
  import type { Thread } from '../../types/models';
  import { formatTokens } from '../../utils/format';
  import { openSettingsOverlay } from '../../stores/settingsOverlay.svelte';
  import { providerSettingsSection } from '../settings/sections';
  import SlidersHorizontal from '@lucide/svelte/icons/sliders-horizontal';
  import Icon from '../primitives/Icon.svelte';
  import { useHoverPopover } from './hoverPopover.svelte';
  import MeterRing, { METER_BUTTON_CLASS } from './MeterRing.svelte';
  import Popover from '../primitives/Popover.svelte';
  import ContextBreakdown from './ContextBreakdown.svelte';

  let { data, thread }: { data: ContextWindow; thread?: Thread | null } = $props();

  let buttonEl: HTMLButtonElement | undefined = $state(undefined);
  // Expanding the breakdown is a click, not a hover: it costs a control
  // round-trip to the live CLI plus a full re-tokenization of the
  // conversation, so it must stay user-initiated. Hovering the meter is
  // free and keeps showing the streaming estimate.
  let breakdownOpen = $state(false);
  const popover = useHoverPopover();

  // Closing the popover unmounts the breakdown, which is what keeps the
  // exact reading from outliving the moment it was taken (Core Principle 2
  // — no cached copy of provider state). Reopening re-reads.
  $effect(() => {
    if (!popover.show) breakdownOpen = false;
  });

  // Codex reports context as a running token count with no categories, so
  // there is nothing to expand there.
  let canReadBreakdown = $derived(thread?.provider === 'claude');

  let maxTokens = $derived(data.maxTokens ?? 0);
  // Trust the wire `usedPercentage`. The canonical normalizer lives in
  // `stores/threadContextWindow.ts` (provider-aware, clamps NaN/±Infinity
  // / negative); by the time data reaches this component it has been
  // through that pipeline. Don't recompute `usedTokens / maxTokens` here —
  // it would silently undo the Codex baseline correction.
  // The Number.isFinite guard only defends the display against a
  // normalizer bug: without it a non-finite value would render a
  // literal "NaN"/"Infinity" ring label and aria text.
  let percentage = $derived(
    Number.isFinite(data.usedPercentage) ? (data.usedPercentage as number) : 0,
  );

  let exceeded = $derived(data.exceeded === true);

  let strokeColor = $derived(
    exceeded || percentage > 95 ? 'stroke-error' : percentage > 80 ? 'stroke-warning' : 'stroke-fg-subtle',
  );

  let displayPct = $derived(Math.round(percentage));
  // Keep the small numeric label readable; the popover spells out
  // "exceeded" so the precise wire signal isn't lost.
  let displayLabel = $derived(exceeded ? 'MAX' : `${displayPct}`);
  let ariaLabel = $derived(
    exceeded ? 'Context Window: exceeded (model returned ContextWindowExceeded)' : `Context Window: ${displayPct}% used`,
  );

  function openContextSettings(): void {
    openSettingsOverlay(thread ? providerSettingsSection(thread.provider) : 'claude');
    popover.show = false;
  }
</script>

<button
  bind:this={buttonEl}
  type="button"
  class={METER_BUTTON_CLASS}
  aria-label={ariaLabel}
  onmouseenter={popover.open}
  onpointerdown={popover.pointerDown}
  onclick={popover.open}
  onmouseleave={popover.scheduleClose}
  onfocus={popover.open}
  onblur={popover.scheduleClose}
>
  <MeterRing label={displayLabel} {percentage} strokeClass={strokeColor} />
</button>

<Popover
  anchor={buttonEl}
  open={popover.show}
  onClose={() => (popover.show = false)}
  placement="top-end"
  role="none"
>
  {#snippet children()}
    <div
      role="tooltip"
      onmouseenter={popover.open}
      onmouseleave={popover.scheduleClose}
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
      <p class="mb-1.5 pr-7 text-[0.625rem] font-semibold text-fg-subtle uppercase tracking-wider">Context window</p>
      <div class="space-y-0.5 text-xs text-fg-muted">
        {#if exceeded}
          <p class="text-error">Context window exceeded</p>
        {:else}
          <p>{displayPct}% used</p>
        {/if}
        <p>{formatTokens(data.usedTokens)}{maxTokens > 0 ? ` / ${formatTokens(maxTokens)}` : ''} tokens</p>
        {#if data.autoCompactPercent}
          <p class="text-fg-hint">
            Compact at {data.autoCompactPercent}%{data.autoCompactTokenLimit ? ` (${formatTokens(data.autoCompactTokenLimit)})` : ''}
          </p>
        {/if}
      </div>
      {#if canReadBreakdown && thread}
        {#if breakdownOpen}
          <ContextBreakdown threadId={thread.id} />
        {:else}
          <button
            type="button"
            onclick={() => (breakdownOpen = true)}
            class="mt-1.5 text-xs text-fg-hint hover:text-fg underline underline-offset-2 decoration-dotted cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 rounded-[var(--radius-field)]"
          >
            Show exact breakdown
          </button>
        {/if}
      {/if}
    </div>
  {/snippet}
</Popover>
