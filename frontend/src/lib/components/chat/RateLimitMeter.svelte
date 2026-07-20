<script lang="ts">
  import { formatResetCountdown } from '../../utils/format';
  import { getProviderAccount } from '../../stores/accountInfo.svelte';
  import { getProviderRateLimit } from '../../stores/rateLimitsInfo.svelte';
  import type { ProviderID } from '../../types/providers';
  import { useHoverPopover } from './hoverPopover.svelte';
  import MeterRing, { METER_BUTTON_CLASS } from './MeterRing.svelte';
  import Popover from '../primitives/Popover.svelte';

  // The ring face shows a static window label ("5h"/"7d"); the
  // percentage and reset countdown live in the popover. `windowMins`
  // identifies the window slot to look up — both the label and the
  // popover header derive from it so the two can never drift.
  //
  // `provider` keys into the global rate-limits store
  // (`rateLimitsInfo.svelte.ts`). The store is account-scoped, so a
  // freshly switched-to thread shows the same data its sibling thread
  // had. When provider is undefined (transient state during thread
  // setup) the entry resolves to null and the ring renders empty.
  let {
    windowMins,
    provider,
  }: {
    windowMins: number;
    provider?: ProviderID;
  } = $props();

  // Re-runs whenever the global store's Map identity flips on
  // `setProviderRateLimits`. Empty/missing → null → empty ring.
  let entry = $derived(getProviderRateLimit(provider, windowMins));

  let buttonEl: HTMLButtonElement | undefined = $state(undefined);
  // Computed on each hover-open so the displayed countdown is fresh.
  // A `$derived` over `entry.resetsAt` would only invalidate when the
  // wire pushes a new value — meaning a popover hover an hour later
  // would still show the original "Resets in 1h" string.
  let countdownText = $state('');
  const popover = useHoverPopover(() => {
    countdownText = entry ? formatResetCountdown(entry.resetsAt) : '';
  });

  let label = $derived(
    windowMins === 300
      ? '5h'
      : windowMins === 10080
        ? '7d'
        : `${Math.max(1, Math.round(windowMins / 60))}h`,
  );
  let popoverHeader = $derived(
    windowMins === 300 ? '5-hour limit' : windowMins === 10080 ? '7-day limit' : `${label} limit`,
  );

  // Number.isFinite filters out NaN / Infinity so a wire glitch can't
  // leak NaN into the threshold palette, displayPct, or the aria label.
  // (MeterRing guards its own dashoffset math independently.)
  let percentage = $derived(
    entry && Number.isFinite(entry.usedPercent)
      ? Math.max(0, Math.min(100, entry.usedPercent))
      : 0,
  );

  // Same threshold palette as ContextWindowMeter — percent-only,
  // provider-agnostic. Claude's status field intentionally does not
  // override; rings stay visually consistent with Codex.
  let strokeColor = $derived(
    !entry
      ? 'stroke-fg-subtle'
      : percentage > 95
        ? 'stroke-error'
        : percentage > 80
          ? 'stroke-warning'
          : 'stroke-fg-subtle',
  );

  let displayPct = $derived(Math.round(percentage));

  // Pull plan info (subscriptionType) out of the global accountInfo store
  // when a provider is known. The store is reactive — calling
  // `getProviderAccount` inside `$derived` re-runs when the underlying
  // Map identity flips on `setProviderAccount`.
  let planLabel = $derived.by(() => {
    if (!provider) return '';
    const acc = getProviderAccount(provider);
    return acc?.subscriptionType ?? '';
  });
</script>

<button
  bind:this={buttonEl}
  type="button"
  class={METER_BUTTON_CLASS}
  aria-label={entry
    ? `${popoverHeader}: ${displayPct}% used`
    : `${popoverHeader}: awaiting first update`}
  onmouseenter={popover.open}
  onmouseleave={popover.scheduleClose}
  onfocus={popover.open}
  onblur={popover.scheduleClose}
>
  <MeterRing {label} {percentage} strokeClass={strokeColor} showArc={!!entry} />
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
      class="relative bg-surface-1 border border-border-subtle rounded-[var(--radius-control)] shadow-menu px-3 py-2 min-w-[170px]"
    >
      <p class="mb-1.5 text-[0.625rem] font-semibold text-fg-subtle uppercase tracking-wider">{popoverHeader}</p>
      <div class="space-y-0.5 text-xs text-fg-muted">
        {#if entry}
          <p>{displayPct}% used</p>
          {#if countdownText}
            <p class="text-fg-hint">{countdownText}</p>
          {/if}
        {:else}
          <p class="text-fg-hint">Awaiting first update…</p>
        {/if}
        {#if planLabel}
          <p class="text-fg-hint">Plan: {planLabel}</p>
        {/if}
      </div>
    </div>
  {/snippet}
</Popover>
