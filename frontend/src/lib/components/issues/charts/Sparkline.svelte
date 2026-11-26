<script lang="ts">
  // A number tile's trend: the value of each period of the range, oldest
  // first, as a small line to the tile's measured width. Decorative: the
  // tile says the value and its change in words, and opens its rows.
  import { numValue, type Num } from '../../../stores/trackerDashboardModel';
  import { valueScale } from './chartScale';

  interface Props {
    points: readonly Num[];
  }

  let { points }: Props = $props();
  let width = $state(0);
  const HEIGHT = 24;
  const top = $derived(valueScale(Math.max(0, ...points.map((p) => numValue(p) ?? 0))).max);
  const d = $derived.by(() => {
    let out = '';
    let pen = false;
    points.forEach((p, i) => {
      const v = numValue(p);
      if (v === null) {
        pen = false;
        return;
      }
      const x = points.length <= 1 ? width / 2 : 2 + (i / (points.length - 1)) * (width - 4);
      const y = HEIGHT - 2 - (v / top) * (HEIGHT - 4);
      out += `${pen ? 'L' : 'M'}${x},${y}`;
      pen = true;
    });
    return out;
  });
</script>

<div class="w-full" bind:clientWidth={width} aria-hidden="true" data-testid="dashboard-sparkline">
  {#if width > 0 && points.length > 1}
    <svg width={width} height={HEIGHT} class="block">
      <path {d} fill="none" stroke="var(--chart-series-1)" stroke-width="2" stroke-linejoin="round" stroke-linecap="round" />
    </svg>
  {/if}
</div>
