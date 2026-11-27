// The geometry the dashboard's charts share: a value axis rounded to
// readable ticks, bands for bars and points across a measured width, and
// how many category labels fit. Pure, so the charts draw to whatever width
// their box measures and the rules are tested here.

import { formatAmount, inTime, type DashboardUnit } from '../../../utils/dashboardAmount';

/** A value axis from 0: its top and its ticks, each a 1, 2 or 5 step. */
export interface ValueScale {
  readonly max: number;
  readonly ticks: readonly number[];
}

/** The axis for values up to `top`, about `count` ticks; a top of 0 or
 * less (nothing to draw) is an axis to 1. The ticks are counted, not
 * stepped to the top, so a top near the largest number, whose rounded
 * axis would not be finite, is an axis of two ticks to `top`. */
export function valueScale(top: number, count = 4): ValueScale {
  if (!(top > 0) || !Number.isFinite(top)) return { max: 1, ticks: [0, 1] };
  const rough = top / count;
  const magnitude = 10 ** Math.floor(Math.log10(rough));
  const step = [1, 2, 5, 10].map((m) => m * magnitude).find((s) => s >= rough) ?? 10 * magnitude;
  const steps = Math.ceil(top / step);
  const max = steps * step;
  if (!Number.isFinite(max) || !(step > 0) || steps > 2 * count) return { max: top, ticks: [0, top] };
  return { max, ticks: Array.from({ length: steps + 1 }, (_, i) => Math.round(i * step * 1e6) / 1e6) };
}

/** A chart's value axis in the unit its values read in: valueScale's
 * ticks in hours for time (inTime), so a step is a whole 1, 2 or 5 hours
 * or a part of one, each tick labeled as the chart's numbers are. */
export interface UnitScale {
  readonly max: number;
  readonly ticks: readonly { readonly value: number; readonly label: string }[];
}

export function unitScale(top: number, unit: DashboardUnit, estimates: string): UnitScale {
  const per = inTime(unit, estimates) ? 3600 : 1;
  const s = valueScale(top / per);
  return { max: s.max * per, ticks: s.ticks.map((t) => ({ value: t * per, label: formatAmount(t * per, unit, estimates) })) };
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
