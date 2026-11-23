<script lang="ts">
  // Last-known quota bars for one saved account. Lives in shared/ because it
  // has two feature homes: the Accounts cards on a provider's settings page
  // and the account-switcher picker.
  //
  // `lineEnd` is a control both homes place the same way: at the end of the
  // block's last line, which wraps it onto a line of its own when it does
  // not fit beside that line's text. The last line is one element that
  // outlives every change of the limits under it, placed in the grid where
  // the last limit's countdown goes, so the control is mounted once and keeps
  // what it has open. Its countdown reserves the width of the widest text
  // that limit's window can show, so whether the control fits beside it does
  // not change as the countdown runs. A card that passes none renders exactly
  // as before.
  import type { Snippet } from 'svelte';
  import { rateLimitDisplayName, rateLimitWindowLabel } from '../../stores/rateLimitsInfo.svelte';
  import type { RateLimitEntry } from '../../types/events';
  import { formatResetCountdown } from '../../utils/format';
  import { uniqueEachKeys } from '../../utils/uniqueEachKeys';

  let { limits, lineEnd }: { limits: RateLimitEntry[]; lineEnd?: Snippet } = $props();

  // limitId is provider-reported; nothing upstream promises the wire
  // never repeats a (limitId, windowMins) pair, and a repeated key in a
  // keyed `{#each}` throws `each_key_duplicate` mid-flush
  // (utils/uniqueEachKeys.ts).
  const limitKeys = $derived(
    uniqueEachKeys(limits, (limit) => `${limit.limitId}:${limit.windowMins}`),
  );

  function limitLabel(limit: RateLimitEntry): string {
    const name = rateLimitDisplayName(limit);
    if (limit.windowMins <= 0) return name || 'Usage limit';
    const window = rateLimitWindowLabel(limit.windowMins);
    if (!name || name.toLowerCase() === window.toLowerCase()) return window;
    // Avoid duplicating the window when the provider's LimitName already
    // embeds it (e.g. Antigravity's "Gemini · 5-hour" / "Gemini · Weekly").
    const lower = name.toLowerCase();
    if (
      (limit.windowMins === 300 && (lower.includes('5-hour') || lower.includes('5h'))) ||
      (limit.windowMins === 10080 && (lower.includes('weekly') || lower.includes('7d')))
    ) {
      return name;
    }
    return `${name} · ${window}`;
  }

  // Where limit `index` sits in the placed grid: its bar in one row and its
  // countdown in the row under it, one column below `sm` and two above. A
  // countdown sits at the top of its row (self-start), so the last line
  // wrapping beside it leaves its box as it was.
  function place(index: number): { row: number; smRow: number; smCol: number } {
    return { row: 2 * index + 1, smRow: 2 * Math.floor(index / 2) + 1, smCol: (index % 2) + 1 };
  }

  const digits = (n: number) => '0'.repeat(String(Math.max(0, Math.floor(n))).length);

  /**
   * The widest text formatResetCountdown can give a reset inside `limit`'s
   * window, one string per form it can take. Rendered with tabular digits,
   * so a zero is as wide as any digit. A window of unknown length uses the
   * day count it shows now.
   */
  function countdownSizers(limit: RateLimitEntry): string[] {
    const sizers = ['Resets in <1m', 'Resets in 00m'];
    const minutes = limit.windowMins > 0
      ? limit.windowMins
      : Math.max(0, (limit.resetsAt * 1000 - Date.now()) / 60_000);
    if (minutes >= 60 || limit.windowMins <= 0) sizers.push(`Resets in ${digits(Math.min(23, minutes / 60))}h 00m`);
    if (minutes >= 24 * 60) sizers.push(`Resets in ${digits(minutes / (24 * 60))}d 00h`);
    return sizers;
  }

  let last = $derived(limits.length > 0 ? limits[limits.length - 1] : undefined);
  let lastPlace = $derived(place(Math.max(0, limits.length - 1)));
</script>

{#if lineEnd}
  <div class="mt-2 grid gap-x-4 sm:grid-cols-2" data-limits-grid>
    {#each limits as limit, limitIndex (limitKeys[limitIndex] ?? limitIndex)}
      {@const countdown = formatResetCountdown(limit.resetsAt)}
      {@const at = place(limitIndex)}
      <div
        class="min-w-0 [grid-row:var(--row)] sm:[grid-column:var(--sm-col)] sm:[grid-row:var(--sm-row)] {limitIndex === 0
          ? ''
          : limitIndex === 1
            ? 'mt-1.5 sm:mt-0'
            : 'mt-1.5'}"
        style:--row={at.row}
        style:--sm-row={at.smRow}
        style:--sm-col={at.smCol}
      >
        <div class="flex items-center justify-between gap-2 text-[0.65625rem]">
          <span class="truncate text-fg-muted">{limitLabel(limit)}</span>
          <span class="tabular-nums text-fg-hint">{Math.round(limit.usedPercent)}%</span>
        </div>
        <div class="mt-1 h-1 overflow-hidden rounded-full bg-surface-3">
          <div
            class="h-full rounded-full bg-accent"
            style:width={`${Math.max(0, Math.min(100, limit.usedPercent))}%`}
          ></div>
        </div>
      </div>
      {#if countdown && limitIndex < limits.length - 1}
        <p
          class="mt-0.5 self-start text-[0.625rem] text-fg-hint [grid-row:var(--row)] sm:[grid-column:var(--sm-col)] sm:[grid-row:var(--sm-row)]"
          style:--row={at.row + 1}
          style:--sm-row={at.smRow + 1}
          style:--sm-col={at.smCol}
          data-countdown
        >
          {countdown}
        </p>
      {/if}
    {/each}
    <div
      class="flex flex-wrap items-start justify-between gap-x-2 {last
        ? 'mt-0.5 [grid-row:var(--row)] sm:[grid-column:var(--sm-col)] sm:[grid-row:var(--sm-row)]'
        : 'col-span-full row-start-1 pl-4'}"
      style:--row={lastPlace.row + 1}
      style:--sm-row={lastPlace.smRow + 1}
      style:--sm-col={lastPlace.smCol}
      data-limits-last-line
    >
      {#if !last}
        <p class="text-[0.65625rem] text-fg-hint">Usage not checked yet.</p>
      {:else if last.resetsAt > 0}
        <p class="inline-grid text-[0.625rem] tabular-nums text-fg-hint">
          <span class="col-start-1 row-start-1" data-countdown>{formatResetCountdown(last.resetsAt)}</span>
          {#each countdownSizers(last) as sizer (sizer)}
            <span class="invisible col-start-1 row-start-1" aria-hidden="true">{sizer}</span>
          {/each}
        </p>
      {/if}
      {@render lineEnd()}
    </div>
  </div>
{:else if limits.length > 0}
  <div class="mt-2 grid gap-x-4 gap-y-1.5 sm:grid-cols-2">
    {#each limits as limit, limitIndex (limitKeys[limitIndex] ?? limitIndex)}
      {@const countdown = formatResetCountdown(limit.resetsAt)}
      <div class="min-w-0">
        <div class="flex items-center justify-between gap-2 text-[0.65625rem]">
          <span class="truncate text-fg-muted">{limitLabel(limit)}</span>
          <span class="tabular-nums text-fg-hint">{Math.round(limit.usedPercent)}%</span>
        </div>
        <div class="mt-1 h-1 overflow-hidden rounded-full bg-surface-3">
          <div
            class="h-full rounded-full bg-accent"
            style:width={`${Math.max(0, Math.min(100, limit.usedPercent))}%`}
          ></div>
        </div>
        {#if countdown}
          <p class="mt-0.5 text-[0.625rem] text-fg-hint">{countdown}</p>
        {/if}
      </div>
    {/each}
  </div>
{:else}
  <p class="mt-2 pl-4 text-[0.65625rem] text-fg-hint">Usage not checked yet.</p>
{/if}
