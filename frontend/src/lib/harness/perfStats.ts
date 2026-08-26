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
