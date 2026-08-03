<script lang="ts">
  // Codex's OWN account-level token report, rendered under the usage
  // modal's Codex view. Everything above it in the modal is Agent
  // Overflow's ledger — what AO itself observed, priced from a local
  // rate table. This section is the provider's ground truth for the whole
  // login: every turn the account ran anywhere, including the Codex TUI,
  // another editor, or another machine. The two populations differ on
  // purpose, and the subtitle says so.
  //
  // Absence is absence. `GetCodexAccountUsage` answers null for an older
  // codex, an API-key login with no usage profile, and a brand-new
  // account; none of those render a zero. A real failure (spawn, timeout,
  // malformed response) is a visible line rather than a hidden section.
  //
  // Deliberately NOT subscribed to usageRefresh: the backend serves this
  // from a TTL cache and the numbers are a slow-moving provider-side
  // aggregate, so refetching per completed turn would promise a liveness
  // the report cannot deliver.

  import { GetCodexAccountUsage, type CodexAccountUsage } from '../../stores/bindings';
  import { formatTokens } from '../../utils/format';
  import UsageHeatmapGrid from './UsageHeatmapGrid.svelte';
  import { buildHeatmapGrid, type HeatmapCell, type UsageDayBucket } from './heatmapGrid';

  interface Props {
    /** Fetch only while the section is actually shown. */
    enabled: boolean;
  }

  let { enabled }: Props = $props();

  // Same window as the modal's ledger heatmap directly above, so the two
  // grids are read against each other rather than as separate spans.
  const HEATMAP_WEEKS = 26;

  let usage = $state<CodexAccountUsage | null>(null);
  let error = $state<string | null>(null);
  // Fixed at fetch time so the grid's "today" column can't drift from the
  // instant the data describes.
  let nowMs = $state(Date.now());

  $effect(() => {
    if (!enabled) {
      usage = null;
      error = null;
      return;
    }
    let cancelled = false;
    (async () => {
      try {
        const result = await GetCodexAccountUsage();
        if (cancelled) return;
        nowMs = Date.now();
        usage = result;
        error = null;
      } catch (err) {
        if (cancelled) return;
        console.error('codex account usage fetch failed', err);
        usage = null;
        error = err instanceof Error ? err.message : String(err);
      }
    })();
    return () => {
      cancelled = true;
    };
  });

  /** Mirrors the Codex TUI's own `/usage` duration format. */
  function formatDuration(seconds: number): string {
    const total = Math.max(0, Math.trunc(seconds));
    const hours = Math.floor(total / 3600);
    const minutes = Math.floor((total % 3600) / 60);
    if (hours === 0 && minutes === 0) return `${total}s`;
    if (hours === 0) return `${minutes}m`;
    if (minutes === 0) return `${hours}h`;
    return `${hours}h ${minutes}m`;
  }

  /** Current streak, with the record alongside it when they differ —
   *  same shape the Codex TUI prints. */
  function formatStreak(current: number | null, longest: number | null): string | null {
    if (current === null && longest === null) return null;
    if (current === null) return `— (best ${longest}d)`;
    if (longest === null || longest === current) return `${current}d`;
    return `${current}d (best ${longest}d)`;
  }

  interface Tile {
    label: string;
    value: string;
  }

  /** An unreported stat arrives as an absent key, not as null — collapse
   *  both to null so a genuine 0 is still a reported 0. */
  function reported(value: number | null | undefined): number | null {
    return value ?? null;
  }

  // Only reported fields become tiles: the backend genuinely omits values
  // it has no history for, and an omitted stat must not render as 0.
  let tiles: Tile[] = $derived.by(() => {
    const u = usage;
    if (!u) return [];
    const out: Tile[] = [];
    const lifetime = reported(u.lifetimeTokens);
    if (lifetime !== null) out.push({ label: 'Lifetime', value: formatTokens(lifetime) });
    const peak = reported(u.peakDailyTokens);
    if (peak !== null) out.push({ label: 'Peak day', value: formatTokens(peak) });
    const streak = formatStreak(reported(u.currentStreakDays), reported(u.longestStreakDays));
    if (streak) out.push({ label: 'Streak', value: streak });
    const longestTurn = reported(u.longestRunningTurnSec);
    if (longestTurn !== null) out.push({ label: 'Longest task', value: formatDuration(longestTurn) });
    return out;
  });

  // Codex reports one combined token total per day and no cost at all, so
  // the grid's cost axis stays at zero and its token fallback drives the
  // intensity ramp.
  let days: UsageDayBucket[] = $derived(
    (usage?.dailyBuckets ?? []).map((bucket) => ({
      bucket: bucket.startDate,
      costUsd: 0,
      tokens: bucket.tokens,
      unpricedRows: 0,
    })),
  );

  let grid = $derived(buildHeatmapGrid(days, nowMs, HEATMAP_WEEKS));

  function tooltipFor(cell: HeatmapCell): string {
    return `${cell.monthShort} ${cell.dayOfMonth} · ${formatTokens(cell.tokens)} tok`;
  }
</script>

{#if usage || error}
  <div class="flex flex-col gap-1.5" data-testid="usage-codex-account">
    <div class="flex items-baseline justify-between gap-3">
      <h3 class="text-[0.625rem] uppercase tracking-[0.12em] text-fg-subtle">Codex Account</h3>
      {#if usage?.accountEmail}
        <span class="text-[0.625rem] text-fg-subtle truncate">{usage.accountEmail}</span>
      {/if}
    </div>
    {#if error}
      <p class="text-xs text-error" data-testid="usage-codex-account-error">
        Couldn't read Codex's account usage: {error}
      </p>
    {:else}
      <p class="text-xs text-fg-muted">
        Reported by Codex for this login, including usage outside Agent Overflow. The figures
        above are Agent Overflow's own.
      </p>
      {#if tiles.length > 0}
        <!-- Left-aligned rather than spread like the totals row above:
             the tile count varies with what Codex reported, and
             justify-between would push two tiles to opposite edges. -->
        <div class="flex items-start gap-8 flex-wrap">
          {#each tiles as tile (tile.label)}
            <div class="flex flex-col gap-0.5">
              <span class="text-[0.625rem] uppercase tracking-[0.12em] text-fg-subtle whitespace-nowrap">
                {tile.label}
              </span>
              <span
                class="text-sm font-medium text-fg tabular-nums whitespace-nowrap"
                data-testid="usage-codex-account-value"
              >
                {tile.value}
              </span>
            </div>
          {/each}
        </div>
      {/if}
      {#if days.length > 0}
        <UsageHeatmapGrid columns={grid} tooltip={tooltipFor} cellTestId="usage-codex-account-cell" />
      {/if}
    {/if}
  </div>
{/if}
