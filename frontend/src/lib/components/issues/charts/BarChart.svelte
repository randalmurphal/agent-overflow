<script lang="ts">
  // A dashboard's bars (docs/specs/issues.md, "Dashboards"): one band per
  // bucket, a bar per series side by side, drawn to the width the box
  // measures. Vertical for periods, horizontal for people, epics and text,
  // whose label narrows the page when the bucket can. A bar's value shows
  // on hover and focus; a press or Enter opens its rows.
  import type { BucketValue } from '../../../../../bindings/agent-overflow/internal/trackerdash/models';
  import { formatNum, numValue, type DashboardUnit, type Num } from '../../../stores/trackerDashboardModel';
  import { band, labelEvery, SERIES_COLORS, valueScale } from './chartScale';

  interface Props {
    title: string;
    buckets: readonly BucketValue[];
    series: readonly string[];
    horizontal: boolean;
    unit: DashboardUnit;
    estimates: string;
    onPick: (bucket: BucketValue, series: string) => void;
    /** A bucket's label narrows the page to it; none when it cannot. */
    onLabel?: (bucket: BucketValue) => void;
  }

  let { title, buckets, series, horizontal, unit, estimates, onPick, onLabel }: Props = $props();

  let width = $state(0);
  let hovered = $state<{ b: number; s: number } | null>(null);
  const named = $derived(series.length > 1 || series[0] !== '');
  const top = $derived(Math.max(0, ...buckets.flatMap((b) => b.cells.map((c) => numValue(c as Num) ?? 0))));
  const scale = $derived(valueScale(top));

  const V_HEIGHT = 168;
  const V_LEFT = 36;
  const V_BOTTOM = 22;
  const H_ROW = 24;

  const plotW = $derived(Math.max(0, width - V_LEFT));
  const plotH = V_HEIGHT - V_BOTTOM - 8;
  const every = $derived(labelEvery(buckets.length, plotW));
  const hLabelW = $derived(Math.min(160, Math.floor(width * 0.4)));
  const hPlotW = $derived(Math.max(0, width - hLabelW - 56));

  function cellText(b: BucketValue, s: number): string {
    const name = named ? `${series[s]}: ` : '';
    return `${b.label}, ${name}${formatNum(b.cells[s] as Num, unit, estimates)}`;
  }

  /** A bar's path from the baseline, its far end rounded. */
  function vBar(x: number, w: number, h: number): string {
    const r = Math.min(4, w / 2, h);
    const y0 = plotH + 8;
    const y1 = y0 - h;
    return `M${x},${y0}V${y1 + r}Q${x},${y1} ${x + r},${y1}H${x + w - r}Q${x + w},${y1} ${x + w},${y1 + r}V${y0}Z`;
  }

  function hBar(y: number, h: number, w: number): string {
    const r = Math.min(4, h / 2, w);
    return `M0,${y}H${w - r}Q${w},${y} ${w},${y + r}V${y + h - r}Q${w},${y + h} ${w - r},${y + h}H0Z`;
  }

  function key(event: KeyboardEvent, b: BucketValue, s: number): void {
    if (event.key !== 'Enter' && event.key !== ' ') return;
    event.preventDefault();
    onPick(b, named ? series[s] : '');
  }
</script>

<div class="relative w-full" bind:clientWidth={width} data-testid="dashboard-bars" role="group" aria-label={title}>
  {#if named}
    <div class="mb-1 flex flex-wrap gap-3 text-[0.6875rem] text-fg-muted" data-testid="dashboard-legend">
      {#each series as name, s (name)}
        <span class="inline-flex items-center gap-1"><span class="inline-block size-2 rounded-sm" style:background={SERIES_COLORS[s]}></span>{name}</span>
      {/each}
    </div>
  {/if}
  {#if width > 0 && !horizontal}
    <svg width={width} height={V_HEIGHT} class="block overflow-visible" aria-hidden="false">
      {#each scale.ticks as t (t)}
        {@const y = plotH + 8 - (t / scale.max) * plotH}
        <line x1={V_LEFT} x2={width} y1={y} y2={y} class="stroke-border-subtle" stroke-width="1" />
        <text x={V_LEFT - 6} y={y + 3} text-anchor="end" class="fill-fg-subtle text-[0.625rem]">{t}</text>
      {/each}
      <g transform="translate({V_LEFT},0)">
        {#each buckets as b, i (b.key)}
          {@const outer = band(i, buckets.length, plotW, 8)}
          {#each series as _, s (s)}
            {@const inner = band(s, series.length, outer.size, 2)}
            {@const v = numValue(b.cells[s] as Num)}
            {@const h = v === null ? 0 : (v / scale.max) * plotH}
            <g
              role="button"
              tabindex="0"
              aria-label={cellText(b, s)}
              class="cursor-pointer outline-none focus-visible:[&>rect]:stroke-accent"
              data-testid="dashboard-bar"
              data-bucket={b.key}
              data-series={series[s]}
              onclick={() => onPick(b, named ? series[s] : '')}
              onkeydown={(e) => key(e, b, s)}
              onpointerenter={() => { hovered = { b: i, s }; }}
              onpointerleave={() => { hovered = null; }}
              onfocus={() => { hovered = { b: i, s }; }}
              onblur={() => { hovered = null; }}
            >
              <!-- The hit target is the whole band's height, wider than the bar. -->
              <rect x={outer.start + inner.start} y="0" width={inner.size} height={plotH + 8} fill="transparent" stroke-width="2" />
              {#if h > 0}
                <path d={vBar(outer.start + inner.start, inner.size, h)} fill={SERIES_COLORS[s]} opacity={b.partial ? 0.7 : 1} />
              {/if}
            </g>
          {/each}
          {#if i % every === 0}
            <text x={outer.start + outer.size / 2} y={V_HEIGHT - 6} text-anchor="middle" class="fill-fg-muted text-[0.625rem]">{b.label.length > 14 ? `${b.label.slice(0, 13)}…` : b.label}</text>
          {/if}
        {/each}
      </g>
    </svg>
    {#if hovered}
      {@const b = buckets[hovered.b]}
      {@const outer = band(hovered.b, buckets.length, plotW, 8)}
      <div
        class="pointer-events-none absolute z-[2] -translate-x-1/2 rounded-[var(--radius-control)] border border-border-subtle bg-surface-2 px-2 py-1 text-[0.6875rem] text-fg shadow"
        style:left="{V_LEFT + outer.start + outer.size / 2}px"
        style:top="0px"
        role="status"
        data-testid="dashboard-hover"
      >{cellText(b, hovered.s)}{b.partial ? ' (so far)' : ''}</div>
    {/if}
  {:else if width > 0}
    <div class="flex flex-col gap-1">
      {#each buckets as b, i (b.key)}
        <div class="flex items-center gap-2" style:height="{H_ROW}px">
          {#if onLabel && b.key !== ''}
            <button type="button" class="shrink-0 truncate text-left text-[0.75rem] text-fg-muted hover:text-fg hover:underline compact:select-none" style:width="{hLabelW}px" title="Show only {b.label}" data-testid="dashboard-bar-label" onclick={() => onLabel?.(b)}>{b.label}</button>
          {:else}
            <span class="shrink-0 truncate text-[0.75rem] text-fg-muted" style:width="{hLabelW}px">{b.label}</span>
          {/if}
          <svg width={hPlotW} height={H_ROW} class="block shrink-0">
            {#each series as _, s (s)}
              {@const inner = band(s, series.length, H_ROW - 6, 2)}
              {@const v = numValue(b.cells[s] as Num)}
              {@const w = v === null ? 0 : (v / scale.max) * hPlotW}
              <g
                role="button"
                tabindex="0"
                aria-label={cellText(b, s)}
                class="cursor-pointer outline-none"
                data-testid="dashboard-bar"
                data-bucket={b.key}
                data-series={series[s]}
                onclick={() => onPick(b, named ? series[s] : '')}
                onkeydown={(e) => key(e, b, s)}
                onpointerenter={() => { hovered = { b: i, s }; }}
                onpointerleave={() => { hovered = null; }}
                onfocus={() => { hovered = { b: i, s }; }}
                onblur={() => { hovered = null; }}
              >
                <rect x="0" y={3 + inner.start} width={hPlotW} height={inner.size} fill="transparent" />
                {#if w > 0}<path d={hBar(3 + inner.start, inner.size, w)} fill={SERIES_COLORS[s]} />{/if}
              </g>
            {/each}
          </svg>
          <span class="w-12 shrink-0 text-right text-[0.6875rem] text-fg-muted">{formatNum(b.total as Num, unit, estimates)}</span>
        </div>
      {/each}
      <!-- The hovered bar's value, on a line of its own that is always there,
           so nothing moves when a bar is hovered. -->
      <div class="h-4 truncate text-[0.6875rem] text-fg" role="status" data-testid="dashboard-hover">{hovered ? cellText(buckets[hovered.b], hovered.s) : ''}</div>
    </div>
  {/if}
</div>
