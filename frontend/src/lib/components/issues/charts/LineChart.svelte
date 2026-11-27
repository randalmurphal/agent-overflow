<script lang="ts">
  // A dashboard's line over the range's periods: a 2px line a series, a
  // marker at each value, and a period with no value left as a break in
  // the line, said "None" on hover. Drawn to the measured width; a
  // point's value shows on hover and focus, and a press or Enter opens its
  // rows.
  import type { BucketValue } from '../../../../../bindings/agent-overflow/internal/trackerdash/models';
  import { formatNum, numValue, type Num } from '../../../stores/trackerDashboardModel';
  import type { DashboardUnit } from '../../../utils/dashboardAmount';
  import { isLocalEnter } from '../../../utils/localEnter';
  import { labelEvery, SERIES_COLORS, valueScale } from './chartScale';

  interface Props {
    title: string;
    buckets: readonly BucketValue[];
    series: readonly string[];
    unit: DashboardUnit;
    estimates: string;
    onPick: (bucket: BucketValue, series: string) => void;
  }

  let { title, buckets, series, unit, estimates, onPick }: Props = $props();

  let width = $state(0);
  let hovered = $state<{ b: number; s: number } | null>(null);
  const HEIGHT = 168;
  const LEFT = 36;
  const BOTTOM = 22;
  const TOP = 10;
  const named = $derived(series.length > 1 || series[0] !== '');
  const top = $derived(Math.max(0, ...buckets.flatMap((b) => b.cells.map((c) => numValue(c as Num) ?? 0))));
  const scale = $derived(valueScale(top));
  const plotW = $derived(Math.max(0, width - LEFT - 12));
  const plotH = HEIGHT - BOTTOM - TOP;
  const every = $derived(labelEvery(buckets.length, plotW));

  function x(i: number): number {
    return LEFT + (buckets.length <= 1 ? plotW / 2 : (i / (buckets.length - 1)) * plotW);
  }
  function y(v: number): number {
    return TOP + plotH - (v / scale.max) * plotH;
  }
  /** The line of series s, broken where a period has no value. */
  function path(s: number): string {
    let d = '';
    let pen = false;
    buckets.forEach((b, i) => {
      const v = numValue(b.cells[s] as Num);
      if (v === null) {
        pen = false;
        return;
      }
      d += `${pen ? 'L' : 'M'}${x(i)},${y(v)}`;
      pen = true;
    });
    return d;
  }
  function cellText(b: BucketValue, s: number): string {
    return `${b.label}, ${named ? `${series[s]}: ` : ''}${formatNum(b.cells[s] as Num, unit, estimates)}${b.partial ? ' (so far)' : ''}`;
  }
  // The hovered point's words and where its tip stands, one value the tip
  // reads, so a hover that outlives its bucket draws nothing.
  const tip = $derived.by(() => {
    const b = hovered ? buckets[hovered.b] : undefined;
    return hovered && b ? { text: cellText(b, hovered.s), left: x(hovered.b) } : null;
  });
  function key(event: KeyboardEvent, b: BucketValue, s: number): void {
    if (!isLocalEnter(event) && event.key !== ' ') return;
    event.preventDefault();
    onPick(b, named ? series[s] : '');
  }
</script>

<div class="relative w-full" bind:clientWidth={width} data-testid="dashboard-line" role="group" aria-label={title}>
  {#if named}
    <div class="mb-1 flex flex-wrap gap-3 text-[0.6875rem] text-fg-muted" data-testid="dashboard-legend">
      {#each series as name, s (name)}
        <span class="inline-flex items-center gap-1"><span class="inline-block h-0.5 w-3" style:background={SERIES_COLORS[s]}></span>{name}</span>
      {/each}
    </div>
  {/if}
  {#if width > 0}
    <svg width={width} height={HEIGHT} class="block overflow-visible">
      {#each scale.ticks as t (t)}
        <line x1={LEFT} x2={width} y1={y(t)} y2={y(t)} class="stroke-border-subtle" stroke-width="1" />
        <text x={LEFT - 6} y={y(t) + 3} text-anchor="end" class="fill-fg-subtle text-[0.625rem]">{t}</text>
      {/each}
      {#each series as _, s (s)}
        <path d={path(s)} fill="none" stroke={SERIES_COLORS[s]} stroke-width="2" stroke-linejoin="round" stroke-linecap="round" />
      {/each}
      {#each buckets as b, i (b.key)}
        {#each series as _, s (s)}
          {@const v = numValue(b.cells[s] as Num)}
          <g
            role="button"
            tabindex="0"
            aria-label={cellText(b, s)}
            class="cursor-pointer outline-none"
            data-testid="dashboard-point"
            data-bucket={b.key}
            onclick={() => onPick(b, named ? series[s] : '')}
            onkeydown={(e) => key(e, b, s)}
            onpointerenter={() => { hovered = { b: i, s }; }}
            onpointerleave={() => { hovered = null; }}
            onfocus={() => { hovered = { b: i, s }; }}
            onblur={() => { hovered = null; }}
          >
            <rect x={x(i) - 12} y={TOP} width="24" height={plotH} fill="transparent" />
            {#if v !== null}
              <circle cx={x(i)} cy={y(v)} r={hovered?.b === i && hovered.s === s ? 5 : 4} fill={SERIES_COLORS[s]} class="stroke-surface-0" stroke-width="2" />
            {/if}
          </g>
        {/each}
        {#if i % every === 0}
          <text x={x(i)} y={HEIGHT - 6} text-anchor="middle" class="fill-fg-muted text-[0.625rem]">{b.label.length > 14 ? `${b.label.slice(0, 13)}…` : b.label}</text>
        {/if}
      {/each}
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
  {/if}
</div>
