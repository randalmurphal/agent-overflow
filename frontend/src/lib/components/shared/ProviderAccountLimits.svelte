<script lang="ts">
  // Last-known quota bars for one saved account. Lives in shared/ because it
  // has two feature homes: the Settings → Providers account cards and the
  // account-switcher picker.
  import { rateLimitDisplayName } from '../../stores/rateLimitsInfo.svelte';
  import type { RateLimitEntry } from '../../types/events';
  import { formatResetCountdown } from '../../utils/format';
  import { uniqueEachKeys } from '../../utils/uniqueEachKeys';

  let { limits }: { limits: RateLimitEntry[] } = $props();

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
    const window = limit.windowMins === 300
      ? '5h'
      : limit.windowMins === 10080
        ? '7d'
        : `${Math.max(1, Math.round(limit.windowMins / 60))}h`;
    if (!name || name.toLowerCase() === window.toLowerCase()) return window;
    return `${name} · ${window}`;
  }
</script>

{#if limits.length > 0}
  <div class="mt-2 grid gap-x-4 gap-y-1.5 sm:grid-cols-2">
    {#each limits as limit, limitIndex (limitKeys[limitIndex] ?? limitIndex)}
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
        {#if limit.resetsAt > 0}
          <p class="mt-0.5 text-[0.625rem] text-fg-hint">
            {formatResetCountdown(limit.resetsAt)}
          </p>
        {/if}
      </div>
    {/each}
  </div>
{:else}
  <p class="mt-2 pl-4 text-[0.65625rem] text-fg-hint">Usage not checked yet.</p>
{/if}
