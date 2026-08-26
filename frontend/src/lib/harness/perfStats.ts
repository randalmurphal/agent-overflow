// The pure statistics core behind the harness perf meters (§5 of
// docs/specs/testing-harness.md). No DOM, no timers, no globals: every
// function here takes plain data and returns plain data, which is what
// makes the shape of a perf report assertable in a unit test rather than
// only observable through a browser.
//
// Two accumulators, and the split is about what the question needs:
//
//   * FRAME TIMES want a DISTRIBUTION. p95 and p99 are the whole point of
//     a frame report — a mean hides exactly the stalls a reader is
//     looking for — and a percentile cannot be folded incrementally the
//     way a mean can. Keeping every sample would be unbounded (a ten
//     minute run at 60fps is 36,000 numbers, and the soak rig runs for
//     hours), so this is a fixed 1ms-bucket histogram with an overflow
//     tail: 251 counters, constant memory, and 1ms of quantisation error
//     on a number nobody reads to sub-millisecond precision. The exact
//     max is tracked separately, because the one frame time a reader
//     always wants exactly is the worst one.
//
//   * BUSY TIMES want the same distribution at a FINER grain, plus exact
//     budget-fit counters. See the busy section below for why both.
//
//   * EVERYTHING ELSE (heap bytes, DOM node counts, per-pane row counts)
//     is a slow-moving level, sampled once per backend tick. count / min
//     / max / mean / last is the whole answer and folds in O(1).
//
// Field order in every returned object is fixed and declared in one
// place, because these documents are diffed in a terminal.

/** Frame times at or above this are quantised into the overflow bucket. */
export const FRAME_BUCKET_CEILING_MS = 250;

/** Default threshold for "this frame was long", overridable per run. */
export const DEFAULT_LONG_FRAME_MS = 50;

export interface FrameHistogram {
  /** One counter per whole millisecond, index 0..FRAME_BUCKET_CEILING_MS. */
  readonly buckets: number[];
  count: number;
  sumMs: number;
  maxMs: number;
  longFrames: number;
  longFrameMs: number;
}

export interface FrameSummary {
  frames: number;
  elapsedMs: number;
  fps: number;
  p50Ms: number;
  p95Ms: number;
  p99Ms: number;
  maxMs: number;
  meanMs: number;
  longFrames: number;
  longFrameMs: number;
}

export function newFrameHistogram(longFrameMs = DEFAULT_LONG_FRAME_MS): FrameHistogram {
  return {
    buckets: new Array<number>(FRAME_BUCKET_CEILING_MS + 1).fill(0),
    count: 0,
    sumMs: 0,
    maxMs: 0,
    longFrames: 0,
    longFrameMs: longFrameMs > 0 ? longFrameMs : DEFAULT_LONG_FRAME_MS,
  };
}

/**
 * Folds one inter-frame delta in. Non-finite and negative deltas are
 * dropped rather than clamped: rAF timestamps go backwards across a tab
 * suspend/restore in more than one engine, and a 0ms frame recorded there
 * would pull p50 down precisely in the run that stalled.
 */
export function recordFrame(hist: FrameHistogram, deltaMs: number): void {
  if (!Number.isFinite(deltaMs) || deltaMs < 0) return;
  hist.count += 1;
  hist.sumMs += deltaMs;
  if (deltaMs > hist.maxMs) hist.maxMs = deltaMs;
  if (deltaMs >= hist.longFrameMs) hist.longFrames += 1;
  const bucket = Math.min(FRAME_BUCKET_CEILING_MS, Math.floor(deltaMs));
  hist.buckets[bucket] = (hist.buckets[bucket] ?? 0) + 1;
}

/**
 * Percentile over the bucketed distribution, reported as the bucket's
 * LOWER edge (a frame counted in bucket 16 took at least 16ms). The
 * overflow bucket answers with the exact max, which is the only value in
 * it we actually know.
 */
export function framePercentile(hist: FrameHistogram, fraction: number): number {
  if (hist.count === 0) return 0;
  const target = Math.min(hist.count, Math.max(1, Math.ceil(fraction * hist.count)));
  let seen = 0;
  for (let bucket = 0; bucket < hist.buckets.length; bucket += 1) {
    seen += hist.buckets[bucket] ?? 0;
    if (seen >= target) {
      return bucket >= FRAME_BUCKET_CEILING_MS ? round2(hist.maxMs) : bucket;
    }
  }
  return round2(hist.maxMs);
}

/**
 * fps is frames over WALL CLOCK, not the reciprocal of the mean frame
 * time. They differ exactly when rAF stops firing (a hidden tab, a wedged
 * renderer), and in that case the wall-clock number is the true one:
 * "we painted 4 frames in 3 seconds" is the finding, and a mean over the
 * 4 deltas that were observed would report a healthy 60.
 */
export function summarizeFrames(hist: FrameHistogram, elapsedMs: number): FrameSummary {
  const elapsed = elapsedMs > 0 ? elapsedMs : 0;
  return {
    frames: hist.count,
    elapsedMs: Math.round(elapsed),
    fps: elapsed > 0 ? round2((hist.count * 1000) / elapsed) : 0,
    p50Ms: framePercentile(hist, 0.5),
    p95Ms: framePercentile(hist, 0.95),
    p99Ms: framePercentile(hist, 0.99),
    maxMs: round2(hist.maxMs),
    meanMs: hist.count > 0 ? round2(hist.sumMs / hist.count) : 0,
    longFrames: hist.longFrames,
    longFrameMs: hist.longFrameMs,
  };
}

// ---------------------------------------------------------------------------
// Busy time
//
// The frame histogram above answers "how far apart were the frames", and
// under a vsync-locked compositor that question has exactly one answer: a
// 3ms tick and a 9ms tick both read ~16.7ms. It cannot say whether the work
// FITS a budget, which is the question anyone tuning a 165Hz renderer is
// actually asking, and LoAF cannot either — it only reports frames past
// 50ms, three whole budgets too late.
//
// BUSY TIME is that question's own instrument: how long the main thread was
// occupied by one tick's callbacks, style, layout and paint before it could
// service a cheap task. It is not quantised by vsync, and unlike a frame gap
// it shrinks when the work shrinks.
//
// Two accumulators again, and the split is sharper than the frame one:
//
//   * PERCENTILES fold into a histogram at QUARTER-millisecond buckets,
//     not the frame histogram's whole ones. A 6ms budget read through 1ms
//     buckets reports p50 as an integer, and resolving work below one frame
//     is the entire point of this meter.
//
//   * BUDGET FIT is counted EXACTLY — one counter per budget, incremented
//     at record time, never derived from the buckets. "72% of ticks fit
//     6ms" is the headline number a `--baseline` gates on, and reading it
//     off a bucketed distribution would put a quarter millisecond of
//     quantisation error on the one figure that has to be right.

/** Busy times are bucketed at this resolution. */
export const BUSY_BUCKET_MS = 0.25;

/** Busy times at or above this are quantised into the overflow bucket. */
export const BUSY_BUCKET_CEILING_MS = 250;

const BUSY_BUCKET_COUNT = Math.round(BUSY_BUCKET_CEILING_MS / BUSY_BUCKET_MS) + 1;

/**
 * The budgets a run reports fit against when the caller names none. 6ms is
 * one frame at 165Hz minus the compositor's share, 8ms is 120Hz, 16ms is
 * 60Hz — the three refresh rates a desktop app actually meets.
 */
export const DEFAULT_BUSY_BUDGETS_MS: readonly number[] = [6, 8, 16];

/**
 * How many budgets one run may carry. Every budget is a comparison per
 * measured tick, so this is a bound on the meter's own cost; nobody reads
 * eight columns of fit percentages anyway.
 */
export const MAX_BUSY_BUDGETS = 8;

/**
 * Cleans a caller's budget list: positive and finite only, deduplicated,
 * ASCENDING (recordBusy stops at the first miss, which is only correct on a
 * sorted list), capped. An empty or wholly invalid list falls back to the
 * default rather than producing a report with no fit column at all.
 */
export function normalizeBudgetsMs(budgetsMs?: readonly number[]): number[] {
  const seen = new Set<number>();
  for (const value of budgetsMs ?? []) {
    if (!Number.isFinite(value) || value <= 0) continue;
    seen.add(value);
  }
  if (seen.size === 0) return [...DEFAULT_BUSY_BUDGETS_MS];
  return [...seen].sort((a, b) => a - b).slice(0, MAX_BUSY_BUDGETS);
}

export interface BusyHistogram {
  /** One counter per BUSY_BUCKET_MS, index 0..BUSY_BUCKET_COUNT-1. */
  readonly buckets: number[];
  count: number;
  sumMs: number;
  maxMs: number;
  /** Ascending, fixed at construction. */
  readonly budgetsMs: number[];
  /** withinBudget[i] counts ticks whose busy time was <= budgetsMs[i]. */
  readonly withinBudget: number[];
  /**
   * Ticks whose measurement was thrown away because the previous tick's
   * probe had not answered yet. Reported rather than hidden: a run that
   * dropped most of its ticks measured a different thing than it claims.
   */
  dropped: number;
}

/** One budget's verdict: how many ticks fit, and what share that is. */
export interface BusyBudgetFit {
  budgetMs: number;
  withinTicks: number;
  withinPct: number;
}

export interface BusySummary {
  ticks: number;
  dropped: number;
  p50Ms: number;
  p95Ms: number;
  maxMs: number;
  meanMs: number;
  budgets: BusyBudgetFit[];
}

export function newBusyHistogram(budgetsMs?: readonly number[]): BusyHistogram {
  const budgets = normalizeBudgetsMs(budgetsMs);
  return {
    buckets: new Array<number>(BUSY_BUCKET_COUNT).fill(0),
    count: 0,
    sumMs: 0,
    maxMs: 0,
    budgetsMs: budgets,
    withinBudget: new Array<number>(budgets.length).fill(0),
    dropped: 0,
  };
}

/**
 * Folds one busy measurement in. Same admission rule as recordFrame:
 * non-finite and negative are dropped rather than clamped, because a
 * clock that went backwards must not manufacture a 0ms tick that fits
 * every budget.
 */
export function recordBusy(hist: BusyHistogram, busyMs: number): void {
  if (!Number.isFinite(busyMs) || busyMs < 0) return;
  hist.count += 1;
  hist.sumMs += busyMs;
  if (busyMs > hist.maxMs) hist.maxMs = busyMs;
  const bucket = Math.min(BUSY_BUCKET_COUNT - 1, Math.floor(busyMs / BUSY_BUCKET_MS));
  hist.buckets[bucket] = (hist.buckets[bucket] ?? 0) + 1;
  // Ascending budgets, so the first budget this tick FITS is also the
  // smallest, and every budget after it fits by construction.
  for (let i = 0; i < hist.budgetsMs.length; i += 1) {
    if (busyMs > (hist.budgetsMs[i] ?? 0)) continue;
    for (let j = i; j < hist.withinBudget.length; j += 1) {
      hist.withinBudget[j] = (hist.withinBudget[j] ?? 0) + 1;
    }
    break;
  }
}

/** Records a tick whose measurement could not be attributed to one tick. */
export function recordBusyDrop(hist: BusyHistogram): void {
  hist.dropped += 1;
}

/**
 * Percentile over the bucketed distribution, reported as the bucket's
 * LOWER edge. The overflow bucket answers with the exact max, which is the
 * only value in it we know.
 */
export function busyPercentile(hist: BusyHistogram, fraction: number): number {
  if (hist.count === 0) return 0;
  const target = Math.min(hist.count, Math.max(1, Math.ceil(fraction * hist.count)));
  let seen = 0;
  for (let bucket = 0; bucket < hist.buckets.length; bucket += 1) {
    seen += hist.buckets[bucket] ?? 0;
    if (seen >= target) {
      return bucket >= BUSY_BUCKET_COUNT - 1 ? round2(hist.maxMs) : round2(bucket * BUSY_BUCKET_MS);
    }
  }
  return round2(hist.maxMs);
}

/**
 * A run with no measured tick answers 0% fit rather than 100%: "every tick
 * fit the budget" is a claim, and a meter that measured nothing has not
 * earned it. `ticks: 0` is what a reader (and the CLI's unmeasured rule)
 * checks to tell the two apart.
 */
export function summarizeBusy(hist: BusyHistogram): BusySummary {
  return {
    ticks: hist.count,
    dropped: hist.dropped,
    p50Ms: busyPercentile(hist, 0.5),
    p95Ms: busyPercentile(hist, 0.95),
    maxMs: round2(hist.maxMs),
    meanMs: hist.count > 0 ? round2(hist.sumMs / hist.count) : 0,
    budgets: hist.budgetsMs.map((budgetMs, i) => {
      const within = hist.withinBudget[i] ?? 0;
      return {
        budgetMs,
        withinTicks: within,
        withinPct: hist.count > 0 ? round2((within * 100) / hist.count) : 0,
      };
    }),
  };
}

export interface Series {
  count: number;
  min: number;
  max: number;
  mean: number;
  last: number;
  /** Running total, kept so mean folds in O(1). Serialised too — a report is read, not re-folded, and hiding it would just make the mean unverifiable. */
  sum: number;
}

export function newSeries(): Series {
  return { count: 0, min: 0, max: 0, mean: 0, last: 0, sum: 0 };
}

export function recordSeries(series: Series, value: number): void {
  if (!Number.isFinite(value)) return;
  series.count += 1;
  series.sum += value;
  series.last = value;
  series.mean = round2(series.sum / series.count);
  if (series.count === 1) {
    series.min = value;
    series.max = value;
    return;
  }
  if (value < series.min) series.min = value;
  if (value > series.max) series.max = value;
}

/** Two decimals is the reporting precision everywhere in this module. */
export function round2(value: number): number {
  if (!Number.isFinite(value)) return 0;
  return Math.round(value * 100) / 100;
}
