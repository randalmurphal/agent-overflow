<script lang="ts">
  import { formatResetCountdown } from '../../utils/format';
  import { getProviderAccount } from '../../stores/accountInfo.svelte';
  import {
    getProviderRateLimitsForWindow,
    rateLimitDisplayName,
  } from '../../stores/rateLimitsInfo.svelte';
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

  // The recognized provider-wide default remains the sole source for the
  // ring. Additional entries only enrich the hover card, so a scoped
  // Fable/Spark quota can never make the account-wide ring look exhausted.
  let limitGroup = $derived(getProviderRateLimitsForWindow(provider, windowMins));
  let entries = $derived(limitGroup.limits);
  let entry = $derived(limitGroup.primary);

  let buttonEl: HTMLButtonElement | undefined = $state(undefined);
  // Touched on every hover-open so countdowns recompute against current wall
  // time even when the provider has not pushed a new snapshot.
  let countdownGeneration = $state(0);
  const popover = useHoverPopover(() => {
    countdownGeneration += 1;
  });

  let label = $derived(
    windowMins === 300
      ? '5h'
      : windowMins === 10080
        ? '7d'
        : `${Math.max(1, Math.round(windowMins / 60))}h`,
  );
  let popoverHeader = $derived.by(() => {
    const base = windowMins === 300 ? '5-hour' : windowMins === 10080 ? '7-day' : label;
    return `${base} ${entries.length > 1 ? 'limits' : 'limit'}`;
  });

  // Number.isFinite filters out NaN / Infinity so a wire glitch can't
  // leak NaN into the threshold palette, displayPct, or the aria label.
  // (MeterRing guards its own dashoffset math independently.)
  function normalizedPercentage(usedPercent: number): number {
    return Number.isFinite(usedPercent)
      ? Math.max(0, Math.min(100, usedPercent))
      : 0;
  }

  let percentage = $derived(entry ? normalizedPercentage(entry.usedPercent) : 0);
  let displayedEntries = $derived.by(() => {
    // Explicit dependency: formatResetCountdown reads Date.now(), which is not
    // reactive by itself.
    void countdownGeneration;
    return entries.map((limit) => ({
      key: `${limit.limitId}\u0000${limit.windowMins}`,
      name: rateLimitDisplayName(limit),
      percentage: Math.round(normalizedPercentage(limit.usedPercent)),
      countdown: formatResetCountdown(limit.resetsAt),
    }));
  });

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
      class="relative bg-surface-1 border border-border-subtle rounded-[var(--radius-control)] shadow-menu px-3 py-2 min-w-[210px] max-w-[280px]"
    >
      <p class="mb-1.5 text-[0.625rem] font-semibold text-fg-subtle uppercase tracking-wider">{popoverHeader}</p>
      <div class="text-xs text-fg-muted">
        {#if displayedEntries.length > 0}
          <div class="space-y-2">
            {#each displayedEntries as limit (limit.key)}
              <div>
                <div class="flex items-baseline justify-between gap-4">
                  <p class="min-w-0 truncate" title={limit.name}>{limit.name}</p>
                  <p class="shrink-0 tabular-nums">{limit.percentage}% used</p>
                </div>
                {#if limit.countdown}
                  <p class="mt-0.5 text-fg-hint">{limit.countdown}</p>
                {/if}
              </div>
            {/each}
          </div>
        {:else}
          <p class="text-fg-hint">Awaiting first update…</p>
        {/if}
        {#if planLabel}
          <p class="mt-1.5 text-fg-hint">Plan: {planLabel}</p>
        {/if}
      </div>
    </div>
  {/snippet}
</Popover>
