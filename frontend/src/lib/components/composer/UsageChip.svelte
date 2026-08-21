<script lang="ts">
  // Per-thread usage chip for the composer strip — the "what has this
  // cost" counterpart to ComposerWorkspaceStrip's "where am I" cluster.
  // Hidden until the lifetime usage bucket has data, matching
  // SystemStatsFooter's hide-until-data approach for a fresh thread with
  // no settled turns yet. Click opens a popover with the full token/cost
  // breakdown; the per-model split is fetched lazily on first open since
  // the closed chip never needs it.
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { UsageQuery } from '../../stores/bindings';
  import { getThreadUsageRefreshVersion } from '../../stores/usageRefresh.svelte';
  import { createUsageStats } from '../../stores/usageQuery.svelte';
  import { formatTokens } from '../../utils/format';
  import { displayUsageModelLabel } from '../../utils/modelLabels';
  import { formatUsageCostOrNull } from '../../utils/usageDisplay';
  import { composerTriggerClasses } from './triggerClasses';
  import Popover from '../primitives/Popover.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);

  // Refetch on mount, on thread switch, and whenever a turn completes
  // on THIS thread (per-thread refresh version — see
  // usageRefresh.svelte.ts for why the chip doesn't use the global one).
  const lifetime = createUsageStats(() => {
    const threadId = pane.threadId;
    if (!threadId) return null;
    getThreadUsageRefreshVersion(threadId);
    return new UsageQuery({ threadId });
  });

  let lifetimeBucket = $derived(lifetime.buckets?.[0] ?? null);

  // The per-model breakdown only appears in the popover, so the query
  // stays null (no fetch) until it's open.
  const models = createUsageStats(() => {
    const threadId = pane.threadId;
    if (!open || !threadId) return null;
    return new UsageQuery({ threadId, groupBy: 'model' });
  });

  let modelBuckets = $derived(models.buckets ?? []);
  // `models.buckets` is null both before the popover opens and while a
  // fetch is in flight; once open (and threaded) is true, null means
  // "in flight" specifically.
  let modelBucketsLoading = $derived(open && Boolean(pane.threadId) && models.buckets === null);

  function togglePopover(): void {
    open = !open;
  }

  function closePopover(): void {
    open = false;
  }

  // Output tokens only: input counts re-bill the growing context every
  // turn, so in+out balloons into a number that says little about what
  // the thread produced. The popover's split has the full picture.
  let tokenTotal = $derived(lifetimeBucket?.outputTokens ?? 0);

  // A bucket with unpriced rows carries a partial CostUSD, not a total.
  // formatUsageCostOrNull both omits the segment when there's no priced
  // data at all and prefixes the `≥` lower-bound marker otherwise, so
  // this reads exactly the same as the sidebar footer / usage modal.
  let chipCost = $derived(
    lifetimeBucket ? formatUsageCostOrNull(lifetimeBucket.costUsd, lifetimeBucket.unpricedRows) : null,
  );
  // `costSource` is set only when a PROVIDER priced this thread itself and
  // its figure replaced AO's rate-table arithmetic wholesale (Codex >= 0.148
  // — see app_usage.go's overlay). Empty means the ordinary composition, so
  // the hint appears only when the number on screen is somebody else's.
  let providerEstimated = $derived(lifetimeBucket?.costSource === 'provider-estimate');

  let chipLabel = $derived.by(() => {
    if (!lifetimeBucket) return '';
    const tokens = formatTokens(tokenTotal);
    return chipCost ? `${tokens} · ${chipCost}` : tokens;
  });

  let splitRows = $derived.by(() => {
    if (!lifetimeBucket) return [];
    const rows = [
      { label: 'Input', value: lifetimeBucket.inputTokens },
      { label: 'Output', value: lifetimeBucket.outputTokens },
      { label: 'Cache read', value: lifetimeBucket.cacheReadInputTokens },
      { label: 'Cache write', value: lifetimeBucket.cacheCreationInputTokens },
    ];
    if (lifetimeBucket.reasoningOutputTokens > 0) {
      rows.push({ label: 'Reasoning', value: lifetimeBucket.reasoningOutputTokens });
    }
    return rows;
  });

  let turnCount = $derived(lifetimeBucket?.turnCount ?? 0);

  // Precomputed so the template renders fields instead of calling a
  // formatting function twice per row.
  let modelRows = $derived(
    modelBuckets.map((bucket) => ({
      name: bucket.bucket ? displayUsageModelLabel(bucket.bucket) : 'unknown',
      slug: bucket.bucket,
      tokens: formatTokens(bucket.outputTokens),
      cost: formatUsageCostOrNull(bucket.costUsd, bucket.unpricedRows),
    })),
  );

  function turnLabel(count: number): string {
    return `${count} turn${count === 1 ? '' : 's'}`;
  }
</script>

{#if lifetimeBucket}
  <button
    bind:this={triggerEl}
    type="button"
    onclick={togglePopover}
    aria-haspopup="dialog"
    aria-expanded={open}
    data-testid="usage-chip-trigger"
    title={providerEstimated ? 'Cost estimated by Codex' : undefined}
    class="{composerTriggerClasses} tabular-nums"
  >
    {chipLabel}
  </button>

  <Popover anchor={triggerEl} {open} onClose={closePopover} placement="top-end" role="none">
    {#snippet children()}
      <div
        class="bg-surface-1 border border-border-subtle rounded-[var(--radius-control)] shadow-menu px-3 py-2 min-w-[220px]"
        data-testid="usage-chip-popover"
      >
        <p class="mb-1.5 text-[0.625rem] font-semibold text-fg-subtle uppercase tracking-wider">
          Usage
        </p>
        <div class="space-y-0.5 text-xs text-fg-muted">
          {#each splitRows as row (row.label)}
            <div class="flex items-center justify-between gap-4">
              <span>{row.label}</span>
              <span class="tabular-nums">{formatTokens(row.value)}</span>
            </div>
          {/each}
        </div>

        {#if modelBucketsLoading}
          <p class="mt-2 text-xs text-fg-hint">Loading models…</p>
        {:else if modelRows.length > 0}
          <div class="mt-2 border-t border-border-subtle pt-2 space-y-0.5 text-xs text-fg-muted">
            {#each modelRows as row (row.slug)}
              <div class="flex items-center justify-between gap-4">
                <span class="truncate max-w-[140px]" title={row.slug}>{row.name}</span>
                <span class="tabular-nums shrink-0">
                  {row.tokens}{#if row.cost}
                    &nbsp;· {row.cost}
                  {/if}
                </span>
              </div>
            {/each}
          </div>
        {/if}

        <p
          class="mt-2 border-t border-border-subtle pt-2 text-xs text-fg-hint"
          title={providerEstimated
            ? "The total is Codex's own estimate for this thread. The per-model split above is priced from Agent Overflow's rate table, so the two need not agree."
            : undefined}
        >
          {turnLabel(turnCount)}{#if providerEstimated}&nbsp;· est. by Codex{/if}
        </p>
      </div>
    {/snippet}
  </Popover>
{/if}
