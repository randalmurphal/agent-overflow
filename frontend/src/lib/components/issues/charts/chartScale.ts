// The geometry the dashboard's charts share: a value axis rounded to
// readable ticks, which reaches below 0 when a value does, bands for bars
// and points across a measured width, and how many category labels fit.
// Pure, so the charts draw to whatever width their box measures and the
// rules are tested here.

import { formatAmount, inTime, type DashboardUnit } from '../../../utils/dashboardAmount';

/** A value axis: its ends and its ticks, each a 1, 2 or 5 step. It
 * starts at 0 unless a value is below 0, when it reaches as far below 0
 * as that value by the same step, and 0 is a tick. */
export interface ValueScale {
  readonly min: number;
  readonly max: number;
  readonly ticks: readonly number[];
}

/** The axis for values up to `top`, and down to `bottom` when it is below
 * 0, about `count` ticks; a top of 0 or less with nothing below 0
 * (nothing to draw) is an axis to 1. The ticks are counted, not stepped
 * to the top, so a span near the largest number, whose rounded axis would
 * not be finite, is an axis of its ends and 0. */
export function valueScale(top: number, count = 4, bottom = 0): ValueScale {
  const below = bottom < 0 && Number.isFinite(bottom);
  if (!below && (!(top > 0) || !Number.isFinite(top))) return { min: 0, max: 1, ticks: [0, 1] };
  const hi = top > 0 && Number.isFinite(top) ? top : 0;
  const lo = below ? bottom : 0;
  const ends = lo < 0 && hi > 0 ? [lo, 0, hi] : lo < 0 ? [lo, 0] : [0, hi];
  const whole: ValueScale = { min: lo, max: hi, ticks: ends };
  const rough = (hi - lo) / count;
  if (!Number.isFinite(rough)) return whole;
  const magnitude = 10 ** Math.floor(Math.log10(rough));
  const step = [1, 2, 5, 10].map((m) => m * magnitude).find((s) => s >= rough) ?? 10 * magnitude;
  const up = Math.ceil(hi / step);
  const down = Math.ceil(-lo / step);
  if (!Number.isFinite(up * step) || !Number.isFinite(down * step) || !(step > 0) || up + down > 2 * count) return whole;
  const ticks = Array.from({ length: up + down + 1 }, (_, i) => Math.round((i - down) * step * 1e6) / 1e6);
  return { min: -down * step, max: up * step, ticks };
}

/** A chart's value axis in the unit its values read in, each tick labeled
 * as the chart's numbers are: for time (inTime), valueScale's ticks in
 * whole minutes below an hour, which time reads in to the minute, and in
 * hours from an hour, so no two ticks read the same. */
export interface UnitScale {
  readonly min: number;
  readonly max: number;
  readonly ticks: readonly { readonly value: number; readonly label: string }[];
}

export function unitScale(top: number, unit: DashboardUnit, estimates: string, bottom = 0): UnitScale {
  const reach = Math.max(top, -bottom);
  const per = !inTime(unit, estimates) ? 1 : reach < 3600 ? 60 : 3600;
  let s: ValueScale;
  if (per !== 60) {
    s = valueScale(top / per, 4, bottom / per);
  } else {
    // Whole minutes, and no more ticks than minutes.
    const up = Math.max(Math.ceil(top / per), bottom < 0 ? 0 : 1);
    const down = Math.ceil(-bottom / per);
    s = valueScale(up, Math.min(4, Math.max(1, up + down)), -down);
  }
  return { min: s.min * per, max: s.max * per, ticks: s.ticks.map((t) => ({ value: t * per, label: formatAmount(t * per, unit, estimates) })) };
}

/** A chart's values' ends: the largest, never below 0, and the smallest,
 * never above 0, of the cells that hold a value. */
export function valueEnds(values: readonly (number | null)[]): { top: number; bottom: number } {
  let top = 0;
  let bottom = 0;
  for (const v of values) {
    if (v === null) continue;
    if (v > top) top = v;
    if (v < bottom) bottom = v;
  }
  return { top, bottom };
}

/** Where value `v` sits along `length` on an axis from `scale.min` at 0
 * to `scale.max` at `length`. */
export function along(v: number, scale: { readonly min: number; readonly max: number }, length: number): number {
  return ((v - scale.min) / (scale.max - scale.min)) * length;
}

/** Where band `i` of `n` sits along `length`: its start and its size, with
 * `gap` between bands (the 2px surface gap of adjacent fills). */
export function band(i: number, n: number, length: number, gap = 2): { start: number; size: number } {
  if (n <= 0 || length <= 0) return { start: 0, size: 0 };
  const size = Math.max(1, (length - gap * (n - 1)) / n);
  return { start: i * (size + gap), size };
}

/** Every how many category labels one is drawn, so labels of about
 * `labelPx` each fit along `length`: 1 draws them all. */
export function labelEvery(n: number, length: number, labelPx = 64): number {
  if (n <= 0 || length <= 0) return 1;
  const fit = Math.max(1, Math.floor(length / labelPx));
  return Math.max(1, Math.ceil(n / fit));
}

/** The series colors, by series position, as CSS (theme tokens). A chart
 * draws at most three series (trackerdash maxChartSeries). */
export const SERIES_COLORS: readonly string[] = ['var(--chart-series-1)', 'var(--chart-series-2)', 'var(--chart-series-3)'];
