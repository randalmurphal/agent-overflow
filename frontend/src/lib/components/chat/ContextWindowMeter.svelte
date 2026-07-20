<script lang="ts">
  import type { ContextWindow } from '../../types/events';
  import type { Thread } from '../../types/models';
  import { formatTokens } from '../../utils/format';
  import { OPEN_SETTINGS_EVENT } from '../../stores/events';
  import SlidersHorizontal from 'lucide-svelte/icons/sliders-horizontal';
  import Icon from '../primitives/Icon.svelte';
  import { useHoverPopover } from './hoverPopover.svelte';
  import MeterRing, { METER_BUTTON_CLASS } from './MeterRing.svelte';
  import Popover from '../primitives/Popover.svelte';

  let { data, thread }: { data: ContextWindow; thread?: Thread | null } = $props();

  let buttonEl: HTMLButtonElement | undefined = $state(undefined);
  const popover = useHoverPopover();

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
    if (!thread) return;
    window.dispatchEvent(new CustomEvent(OPEN_SETTINGS_EVENT, {
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
    popover.show = false;
  }
</script>

<button
  bind:this={buttonEl}
  type="button"
  class={METER_BUTTON_CLASS}
  aria-label={ariaLabel}
  onmouseenter={popover.open}
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
    </div>
  {/snippet}
</Popover>
