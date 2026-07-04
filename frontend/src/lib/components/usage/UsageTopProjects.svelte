<script lang="ts">
  // Top-5 projects by usage for the usage modal: groupBy 'project' over
  // the shared period + provider filter. Ranked by cost when any bucket
  // carries a nonzero cost, else by token volume (subscription-less
  // accounts still get a meaningful ranking). Refetches on
  // provider/period change and usageRefresh bumps.

  import { UsageQuery } from '../../stores/bindings';
  import { getUsageRefreshVersion } from '../../stores/usageRefresh.svelte';
  import { getUsagePeriod, periodFromMillis } from '../../stores/usagePeriod.svelte';
  import { createUsageStats, localTzOffsetMinutes } from '../../stores/usageQuery.svelte';
  import { getProject } from '../../stores/projects.svelte';
  import { formatTokens } from '../../utils/format';
  import { formatUsageCostOrNull } from '../../utils/usageDisplay';

  interface Props {
    /** '' = all providers, else 'claude' | 'codex'. */
    provider: string;
  }

  let { provider }: Props = $props();

  const TOP_N = 5;
  const tzOffsetMinutes = localTzOffsetMinutes();

  const stats = createUsageStats(() => {
    const currentProvider = provider;
    const fromMillis = periodFromMillis(getUsagePeriod(), Date.now());
    getUsageRefreshVersion();
    return new UsageQuery({ groupBy: 'project', fromMillis, provider: currentProvider, tzOffsetMinutes });
  });

  let rows = $derived.by(() => {
    const buckets = stats.buckets ?? [];
    const rankByCost = buckets.some((b) => b.costUsd > 0);
    const ranked = [...buckets].sort((a, b) =>
      rankByCost
        ? b.costUsd - a.costUsd
        : b.inputTokens + b.outputTokens - (a.inputTokens + a.outputTokens),
    );
    return ranked.slice(0, TOP_N);
  });

  /** Maps a project-id bucket key to a display name. Empty id = threads
   *  with no project association; a non-empty id absent from the
   *  projects store means the project was since deleted. */
  function projectLabel(id: string): string {
    if (!id) return 'No project';
    const project = getProject(id);
    return project ? project.project.name : '(deleted)';
  }
</script>

<div class="flex flex-col gap-1.5" data-testid="usage-top-projects">
  <h3 class="text-[0.625rem] uppercase tracking-[0.12em] text-fg-subtle">Top Projects</h3>
  {#if rows.length === 0}
    <p class="text-xs text-fg-muted" data-testid="usage-top-projects-empty">No project usage in this period.</p>
  {:else}
    <ul class="flex flex-col">
      {#each rows as row (row.bucket)}
        <li
          class="flex items-center justify-between gap-3 py-1 border-t border-border-subtle first:border-t-0 text-xs"
          data-testid="usage-top-project-row"
        >
          <span class="text-fg truncate min-w-0" data-testid="usage-top-project-name">
            {projectLabel(row.bucket)}
          </span>
          <span class="flex items-center gap-3 shrink-0 tabular-nums">
            <span class="text-fg-muted">{formatTokens(row.inputTokens + row.outputTokens)}</span>
            <span class="text-fg" data-testid="usage-top-project-cost">
              {formatUsageCostOrNull(row.costUsd, row.unpricedRows) ?? '—'}
            </span>
          </span>
        </li>
      {/each}
    </ul>
  {/if}
</div>
