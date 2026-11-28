<script lang="ts">
  // A dashboard's bars (docs/specs/issues.md, "Dashboards"): one band per
  // bucket, a bar per series side by side, drawn to the width the box
  // measures. Vertical for periods, horizontal for people, epics and text,
  // whose label narrows the page when the bucket can. A bar's value shows
  // on hover and focus; a press or Enter opens its rows.
  import type { BucketValue } from '../../../../../bindings/agent-overflow/internal/trackerdash/models';
  import { formatNum, numValue, numWords, type Num } from '../../../stores/trackerDashboardModel';
  import type { DashboardUnit } from '../../../utils/dashboardAmount';
  import { isLocalEnter } from '../../../utils/localEnter';
  import { along, band, labelEvery, SERIES_COLORS, unitScale, valueEnds } from './chartScale';

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
  const ends = $derived(valueEnds(buckets.flatMap((b) => b.cells.map((c) => numValue(c as Num)))));
  const scale = $derived(unitScale(ends.top, unit, estimates, ends.bottom));

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

  // The hovered bar's words and where its tip stands, one value the tip
  // reads, so a hover that outlives its bucket draws nothing.
  const tip = $derived.by(() => {
    const b = hovered ? buckets[hovered.b] : undefined;
    if (!hovered || !b) return null;
    const outer = band(hovered.b, buckets.length, plotW, 8);
    const plain = cellText(b, hovered.s);
    return { plain, text: `${plain}${b.partial ? ' (so far)' : ''}`, left: V_LEFT + outer.start + outer.size / 2 };
  });

  /** Where a value stands up the vertical plot, and 0's line there. */
  function vAt(v: number): number {
    return plotH + 8 - along(v, scale, plotH);
  }
  /** Where a value stands across the horizontal plot. */
  function hAt(v: number): number {
    return along(v, scale, hPlotW);
  }

  /** A bar's path from 0's line to its value at y1, above the line or,
   * for a value below 0, under it, its far end rounded. */
  function vBar(x: number, w: number, y1: number): string {
    const y0 = vAt(0);
    const r = Math.min(4, w / 2, Math.abs(y0 - y1)) * Math.sign(y0 - y1);
    return `M${x},${y0}V${y1 + r}Q${x},${y1} ${x + Math.abs(r)},${y1}H${x + w - Math.abs(r)}Q${x + w},${y1} ${x + w},${y1 + r}V${y0}Z`;
  }

  /** A bar's path from 0's line to its value at x1, right of the line
   * or, for a value below 0, left of it, its far end rounded. */
  function hBar(y: number, h: number, x1: number): string {
    const x0 = hAt(0);
    const r = Math.min(4, h / 2, Math.abs(x1 - x0)) * Math.sign(x1 - x0);
    return `M${x0},${y}H${x1 - r}Q${x1},${y} ${x1},${y + Math.abs(r)}V${y + h - Math.abs(r)}Q${x1},${y + h} ${x1 - r},${y + h}H${x0}Z`;
  }

  function key(event: KeyboardEvent, b: BucketValue, s: number): void {
    if (!isLocalEnter(event) && event.key !== ' ') return;
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
      {#each scale.ticks as t (t.value)}
        {@const y = vAt(t.value)}
        <line x1={V_LEFT} x2={width} y1={y} y2={y} class="stroke-border-subtle" stroke-width="1" />
        <text x={V_LEFT - 6} y={y + 3} text-anchor="end" class="fill-fg-subtle text-[0.625rem]" data-testid="dashboard-axis-tick">{t.label}</text>
      {/each}
      <g transform="translate({V_LEFT},0)">
        {#each buckets as b, i (b.key)}
          {@const outer = band(i, buckets.length, plotW, 8)}
          {#each series as _, s (s)}
            {@const inner = band(s, series.length, outer.size, 2)}
            {@const v = numValue(b.cells[s] as Num)}
            {@const y1 = v === null ? null : vAt(v)}
            <g
              role="button"
              tabindex="0"
              aria-label="{cellText(b, s)}{b.partial ? ' (so far)' : ''}"
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
              {#if y1 !== null && y1 !== vAt(0)}
                <!-- A period still running is outlined in its series color and
                     filled lighter, so its edge keeps the color's contrast. -->
                <path d={vBar(outer.start + inner.start, inner.size, y1)} fill={SERIES_COLORS[s]} fill-opacity={b.partial ? 0.4 : 1} stroke={b.partial ? SERIES_COLORS[s] : 'none'} stroke-width="1.5" data-partial={b.partial ? '' : undefined} data-below={v !== null && v < 0 ? '' : undefined} />
              {/if}
            </g>
          {/each}
          {#if i % every === 0}
            <text x={outer.start + outer.size / 2} y={V_HEIGHT - 6} text-anchor="middle" class="fill-fg-muted text-[0.625rem]">{b.label.length > 14 ? `${b.label.slice(0, 13)}…` : b.label}</text>
          {/if}
        {/each}
      </g>
    </svg>
    {#if tip}
      <div
        class="pointer-events-none absolute z-[2] -translate-x-1/2 rounded-[var(--radius-control)] border border-border-subtle bg-surface-2 px-2 py-1 text-[0.6875rem] text-fg shadow"
        style:left="{tip.left}px"
        style:top="0px"
        role="status"
        data-testid="dashboard-hover"
      >{tip.text}</div>
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
            {#if scale.min < 0}
              <!-- 0's line, which the bars of values below 0 run left of. -->
              <line x1={hAt(0)} x2={hAt(0)} y1="0" y2={H_ROW} class="stroke-border-subtle" stroke-width="1" data-testid="dashboard-bar-zero" />
            {/if}
            {#each series as _, s (s)}
              {@const inner = band(s, series.length, H_ROW - 6, 2)}
              {@const v = numValue(b.cells[s] as Num)}
              {@const x1 = v === null ? null : hAt(v)}
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
                {#if x1 !== null && x1 !== hAt(0)}<path d={hBar(3 + inner.start, inner.size, x1)} fill={SERIES_COLORS[s]} data-below={v !== null && v < 0 ? '' : undefined} />{/if}
              </g>
            {/each}
          </svg>
          <span class="w-12 shrink-0 text-right text-[0.6875rem] text-fg-muted">{numWords(b.total as Num, unit, estimates).value}</span>
        </div>
      {/each}
      <!-- The hovered bar's value, on a line of its own that is always there,
           so nothing moves when a bar is hovered. -->
      <div class="h-4 truncate text-[0.6875rem] text-fg" role="status" data-testid="dashboard-hover">{tip?.plain ?? ''}</div>
    </div>
  {/if}
</div>
