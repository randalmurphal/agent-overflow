// Pure grid-building helper for UsageHeatmap.svelte. ALL date math for
// the heatmap lives here — the component only renders the cells this
// module produces.
//
// Shape: GitHub-style contribution grid. Columns are ISO-ish weeks
// (Monday..Sunday), most recent `weeks` of them, ending at the week
// containing `nowMs`. Cell intensity is a 5-step quantization (0 = no
// data/zero, 1-4 = quartile buckets of the non-zero values) over
// per-day cost, falling back to output-token totals when every bucket's
// cost is zero (subscription-less accounts still paint a heatmap).
// Output only, like every usage surface's token count: input re-bills
// the growing context each turn and would drown the produced work.

/** Minimal per-day bucket shape the grid needs. Structurally compatible
 *  with `UsageBucket` from stores/bindings — callers pass those directly. */
export interface UsageDayBucket {
  /** Local calendar date, "YYYY-MM-DD" (per the query's tzOffsetMinutes). */
  bucket: string;
  costUsd: number;
  outputTokens: number;
  unpricedRows: number;
}

export type HeatmapLevel = 0 | 1 | 2 | 3 | 4;

export interface HeatmapCell {
  /** Local date key, "YYYY-MM-DD". */
  dateKey: string;
  /** 0 = Monday .. 6 = Sunday. */
  weekday: number;
  /** Three-letter month abbreviation for this cell's date (tooltip use). */
  monthShort: string;
  /** Day-of-month for this cell's date (tooltip use). */
  dayOfMonth: number;
  costUsd: number;
  /** Output tokens for the day (the shared usage-surface token metric). */
  tokens: number;
  unpricedRows: number;
  /** True when a bucket exists for this date (distinguishes "zero usage"
   *  from "no row at all" — both currently render at level 0, but the
   *  flag is available for callers that want to tell them apart). */
  hasData: boolean;
  /** Quantized intensity step. 0 = base cell (no data or zero value). */
  level: HeatmapLevel;
  /** True when the date is after `nowMs`'s local day — the current
   *  week's trailing columns before today are not real data. */
  isFuture: boolean;
}

export interface HeatmapColumn {
  /** Date key of this column's Monday. */
  weekStartKey: string;
  /** Three-letter month label, set on the column that contains the 1st
   *  of a month (matches the GitHub contribution-graph convention: the
   *  label marks where the month starts, not the week's own month). */
  monthLabel: string | null;
  /** Exactly 7 cells, Monday..Sunday. */
  cells: HeatmapCell[];
}

const MONTH_LABELS = [
  'Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun',
  'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec',
];

function startOfLocalDay(ms: number): Date {
  const d = new Date(ms);
  d.setHours(0, 0, 0, 0);
  return d;
}

function addDays(d: Date, days: number): Date {
  const next = new Date(d);
  next.setDate(next.getDate() + days);
  return next;
}

/** Monday of the ISO week containing `d` (0=Sun..6=Sat native getDay). */
function mondayOf(d: Date): Date {
  const dow = d.getDay();
  const daysSinceMonday = (dow + 6) % 7;
  return addDays(d, -daysSinceMonday);
}

function toDateKey(d: Date): string {
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, '0');
  const day = String(d.getDate()).padStart(2, '0');
  return `${y}-${m}-${day}`;
}

/**
 * Linear-interpolation percentile (Excel PERCENTILE.INC / R-7 method)
 * over an already-sorted ascending array.
 */
function percentile(sorted: readonly number[], p: number): number {
  if (sorted.length === 1) return sorted[0];
  const idx = p * (sorted.length - 1);
  const lower = Math.floor(idx);
  const upper = Math.ceil(idx);
  if (lower === upper) return sorted[lower];
  const weight = idx - lower;
  return sorted[lower] * (1 - weight) + sorted[upper] * weight;
}

/**
 * Quartile thresholds (Q1, Q2, Q3) of `values`. Empty input returns
 * [0, 0, 0] (the grid then quantizes every cell to level 0).
 */
export function computeQuartiles(values: readonly number[]): [number, number, number] {
  if (values.length === 0) return [0, 0, 0];
  const sorted = [...values].sort((a, b) => a - b);
  return [percentile(sorted, 0.25), percentile(sorted, 0.5), percentile(sorted, 0.75)];
}

function levelFor(value: number, q1: number, q2: number, q3: number): HeatmapLevel {
  if (value <= 0) return 0;
  if (value <= q1) return 1;
  if (value <= q2) return 2;
  if (value <= q3) return 3;
  return 4;
}

/**
 * Builds the heatmap column grid from raw per-day buckets.
 *
 * @param buckets per-day usage rows (any order; duplicates by date are
 *   not expected from the backend and are not deduplicated here).
 * @param nowMs current time in epoch millis — anchors the grid's last
 *   column and marks cells after today's local date as future/blank.
 * @param weeks number of trailing weeks to render (default 26, the
 *   GitHub-style contribution-graph span).
 */
export function buildHeatmapGrid(
  buckets: readonly UsageDayBucket[],
  nowMs: number,
  weeks = 26,
): HeatmapColumn[] {
  const today = startOfLocalDay(nowMs);
  const currentWeekMonday = mondayOf(today);
  const firstWeekMonday = addDays(currentWeekMonday, -7 * (weeks - 1));

  const byDate = new Map<string, UsageDayBucket>();
  for (const b of buckets) byDate.set(b.bucket, b);

  const totalCost = buckets.reduce((sum, b) => sum + b.costUsd, 0);
  const useTokenFallback = totalCost <= 0;

  const metricFor = (b: UsageDayBucket | undefined): number => {
    if (!b) return 0;
    return useTokenFallback ? b.outputTokens : b.costUsd;
  };

  // Enumerate every cell date up front so quartiles are computed over
  // exactly the values the grid will render (not the raw bucket list,
  // which may contain dates outside the visible window).
  const allDates: Date[] = [];
  for (let w = 0; w < weeks; w++) {
    const weekMonday = addDays(firstWeekMonday, w * 7);
    for (let day = 0; day < 7; day++) allDates.push(addDays(weekMonday, day));
  }

  const nonZeroValues = allDates
    .map((d) => metricFor(byDate.get(toDateKey(d))))
    .filter((v) => v > 0);
  const [q1, q2, q3] = computeQuartiles(nonZeroValues);

  const columns: HeatmapColumn[] = [];
  for (let w = 0; w < weeks; w++) {
    const weekMonday = addDays(firstWeekMonday, w * 7);
    const cells: HeatmapCell[] = [];
    let monthLabel: string | null = null;
    for (let day = 0; day < 7; day++) {
      const date = addDays(weekMonday, day);
      const dateKey = toDateKey(date);
      const bucket = byDate.get(dateKey);
      const value = metricFor(bucket);
      if (date.getDate() === 1) monthLabel = MONTH_LABELS[date.getMonth()];
      cells.push({
        dateKey,
        weekday: day,
        monthShort: MONTH_LABELS[date.getMonth()],
        dayOfMonth: date.getDate(),
        costUsd: bucket?.costUsd ?? 0,
        tokens: bucket?.outputTokens ?? 0,
        unpricedRows: bucket?.unpricedRows ?? 0,
        hasData: bucket !== undefined,
        level: levelFor(value, q1, q2, q3),
        isFuture: date.getTime() > today.getTime(),
      });
    }
    columns.push({ weekStartKey: toDateKey(weekMonday), monthLabel, cells });
  }
  return columns;
}
