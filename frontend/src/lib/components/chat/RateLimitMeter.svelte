<script lang="ts">
  import { onDestroy } from 'svelte';
  import { formatResetCountdown } from '../../utils/format';
  import { getProviderAccount } from '../../stores/accountInfo.svelte';
  import { getProviderRateLimit } from '../../stores/rateLimitsInfo.svelte';
  import type { ProviderID } from '../../types/providers';
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
  let showPopover = $state(false);
  // Computed at hover-open time so the displayed countdown is fresh on
  // each open. A `$derived` over `entry.resetsAt` would only invalidate
  // when the wire pushes a new value — meaning a popover hover an hour
  // later would still show the original "Resets in 1h" string.
  let countdownText = $state('');
  let closeTimer: number | null = null;

  // Same geometry as ContextWindowMeter so the rings line up visually.
  const RADIUS = 9.75;
  const CIRCUMFERENCE = 2 * Math.PI * RADIUS;

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
  // produce a NaN dashoffset (which renders as a no-arc circle in some
  // browsers, fooling the visual sanity check).
  let percentage = $derived(
    entry && Number.isFinite(entry.usedPercent)
      ? Math.max(0, Math.min(100, entry.usedPercent))
      : 0,
  );
  let dashOffset = $derived(CIRCUMFERENCE - (percentage / 100) * CIRCUMFERENCE);

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

  function openPopover(): void {
    if (closeTimer !== null) {
      window.clearTimeout(closeTimer);
      closeTimer = null;
    }
    countdownText = entry ? formatResetCountdown(entry.resetsAt) : '';
    showPopover = true;
  }

  function scheduleClose(): void {
    if (closeTimer !== null) window.clearTimeout(closeTimer);
    closeTimer = window.setTimeout(() => {
      showPopover = false;
      closeTimer = null;
    }, 140);
  }

  onDestroy(() => {
    if (closeTimer !== null) window.clearTimeout(closeTimer);
  });
</script>

<button
  bind:this={buttonEl}
  type="button"
  class="relative inline-flex h-8 w-8 items-center justify-center bg-transparent border-none p-0 cursor-help focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded-full hover:bg-surface-2/30 transition-colors"
  aria-label={entry
    ? `${popoverHeader}: ${displayPct}% used`
    : `${popoverHeader}: awaiting first update`}
  onmouseenter={openPopover}
  onmouseleave={scheduleClose}
  onfocus={openPopover}
  onblur={scheduleClose}
>
  <svg class="absolute inset-0 m-auto h-7 w-7 -rotate-90" viewBox="0 0 24 24" aria-hidden="true">
    <circle
      cx="12" cy="12" r={RADIUS}
      fill="none"
      stroke-width="3"
      class="stroke-text-secondary/20"
    />
    {#if entry}
      <circle
        cx="12" cy="12" r={RADIUS}
        fill="none"
        stroke-width="3"
        stroke-linecap="round"
        stroke-dasharray={CIRCUMFERENCE}
        stroke-dashoffset={dashOffset}
        class={strokeColor}
      />
    {/if}
  </svg>
  <span class="absolute left-1/2 top-1/2 block -translate-x-1/2 -translate-y-1/2 text-center text-[0.53125rem] leading-none font-semibold tabular-nums text-text-secondary" aria-hidden="true">
    {label}
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
