import { describe, expect, it } from 'vitest';
import {
  BUSY_BUCKET_CEILING_MS,
  BUSY_BUCKET_MS,
  BUSY_WORST_KEEP,
  DEFAULT_BUSY_BUDGETS_MS,
  FRAME_BUCKET_CEILING_MS,
  MAX_BUSY_BUDGETS,
  framePercentile,
  newBusyHistogram,
  newFrameHistogram,
  newSeries,
  normalizeBudgetsMs,
  recordBusy,
  recordBusyDrop,
  recordFrame,
  recordSeries,
  summarizeBusy,
  summarizeFrames,
} from './perfStats';

describe('frame histogram', () => {
  it('summarises a steady 60fps run', () => {
    const hist = newFrameHistogram(50);
    for (let i = 0; i < 600; i += 1) recordFrame(hist, 16.7);
    const summary = summarizeFrames(hist, 10_000);
    expect(summary.frames).toBe(600);
    expect(summary.fps).toBe(60);
    expect(summary.p50Ms).toBe(16);
    expect(summary.p99Ms).toBe(16);
    expect(summary.longFrames).toBe(0);
    expect(summary.meanMs).toBeCloseTo(16.7, 1);
  });

  it('puts the stalls in the tail without moving the median', () => {
    const hist = newFrameHistogram(50);
    for (let i = 0; i < 99; i += 1) recordFrame(hist, 16);
    recordFrame(hist, 800);
    const summary = summarizeFrames(hist, 2_384);
    expect(summary.p50Ms).toBe(16);
    expect(summary.p95Ms).toBe(16);
    // Nearest-rank over 100 frames puts p99 on the 99th, which is still a
    // 16ms one — the single stall is the 100th. A percentile is a rank, so
    // one outlier in a hundred moves the max and nothing below it; the
    // overflow bucket is not involved at this rank (the case below is).
    expect(summary.p99Ms).toBe(16);
    expect(summary.maxMs).toBe(800);
    expect(summary.longFrames).toBe(1);
  });

  it('reports the overflow bucket as the exact max', () => {
    const hist = newFrameHistogram();
    recordFrame(hist, FRAME_BUCKET_CEILING_MS + 700);
    expect(framePercentile(hist, 0.5)).toBe(950);
  });

  // fps is frames over wall clock, so a run where rAF stopped firing
  // reports the truth rather than the reciprocal of the frames that did.
  it('reports wall-clock fps when rAF stalls', () => {
    const hist = newFrameHistogram();
    for (let i = 0; i < 4; i += 1) recordFrame(hist, 16);
    expect(summarizeFrames(hist, 3_000).fps).toBeCloseTo(1.33, 2);
  });

  it('drops non-finite and negative deltas rather than clamping them', () => {
    const hist = newFrameHistogram();
    recordFrame(hist, Number.NaN);
    recordFrame(hist, -20);
    recordFrame(hist, Number.POSITIVE_INFINITY);
    expect(hist.count).toBe(0);
    expect(summarizeFrames(hist, 1000).fps).toBe(0);
  });

  it('answers an empty run with zeros rather than NaN', () => {
    const summary = summarizeFrames(newFrameHistogram(), 0);
    expect(summary).toMatchObject({ frames: 0, fps: 0, p50Ms: 0, maxMs: 0, meanMs: 0 });
  });
});

describe('busy budgets', () => {
  it('defaults an empty or wholly invalid list rather than reporting no budget', () => {
    expect(normalizeBudgetsMs()).toEqual([...DEFAULT_BUSY_BUDGETS_MS]);
    expect(normalizeBudgetsMs([])).toEqual([...DEFAULT_BUSY_BUDGETS_MS]);
    expect(normalizeBudgetsMs([0, -4, Number.NaN])).toEqual([...DEFAULT_BUSY_BUDGETS_MS]);
  });

  // recordBusy stops at the first budget a tick misses, which is only
  // correct on an ascending list — an unsorted caller would otherwise get
  // silently wrong fit counts rather than an error.
  it('sorts, dedupes and caps what a caller sends', () => {
    expect(normalizeBudgetsMs([16, 6, 8, 6])).toEqual([6, 8, 16]);
    const many = Array.from({ length: MAX_BUSY_BUDGETS + 5 }, (_, i) => i + 1);
    expect(normalizeBudgetsMs(many)).toHaveLength(MAX_BUSY_BUDGETS);
  });
});

describe('busy histogram', () => {
  it('counts budget fit exactly, not off the buckets', () => {
    const hist = newBusyHistogram([6, 8, 16]);
    for (const busy of [1.1, 5.9, 6.0, 6.01, 9, 40]) recordBusy(hist, busy);
    const summary = summarizeBusy(hist);
    expect(summary.ticks).toBe(6);
    expect(summary.budgets).toEqual([
      { budgetMs: 6, withinTicks: 3, withinPct: 50 },
      { budgetMs: 8, withinTicks: 4, withinPct: 66.67 },
      { budgetMs: 16, withinTicks: 5, withinPct: 83.33 },
    ]);
    // 6.01 fits 8 but not 6, which is the distinction a 1ms bucket cannot
    // make and the whole reason the fit counters are not derived from one.
    expect(summary.maxMs).toBe(40);
  });

  it('resolves percentiles below a millisecond', () => {
    const hist = newBusyHistogram([6]);
    for (let i = 0; i < 90; i += 1) recordBusy(hist, 2.6);
    for (let i = 0; i < 10; i += 1) recordBusy(hist, 12.4);
    const summary = summarizeBusy(hist);
    // Bucket lower edge at quarter-millisecond resolution: 2.6 lands in the
    // bucket that starts at 2.5, which a whole-millisecond histogram would
    // have reported as a flat 2.
    expect(summary.p50Ms).toBe(2.5);
    expect(BUSY_BUCKET_MS).toBe(0.25);
    expect(summary.p95Ms).toBe(12.25);
    expect(summary.meanMs).toBeCloseTo(3.58, 2);
  });

  it('reports the overflow bucket as the exact max', () => {
    const hist = newBusyHistogram();
    recordBusy(hist, BUSY_BUCKET_CEILING_MS + 130);
    expect(summarizeBusy(hist).p50Ms).toBe(380);
  });

  // "Every tick fit the budget" is a claim, and a meter that measured
  // nothing has not earned it. A bench reads `ticks` to tell the two apart,
  // so the percentages must not read as a pass.
  it('answers a run that measured nothing with 0%, never 100%', () => {
    const summary = summarizeBusy(newBusyHistogram([6]));
    expect(summary.ticks).toBe(0);
    expect(summary.budgets).toEqual([{ budgetMs: 6, withinTicks: 0, withinPct: 0 }]);
    expect(summary).toMatchObject({ p50Ms: 0, p95Ms: 0, maxMs: 0, meanMs: 0 });
  });

  it('keeps dropped ticks out of the distribution but reports them', () => {
    const hist = newBusyHistogram([6]);
    recordBusy(hist, 3);
    recordBusyDrop(hist);
    recordBusyDrop(hist);
    const summary = summarizeBusy(hist);
    expect(summary).toMatchObject({ ticks: 1, dropped: 2 });
    expect(summary.budgets[0]).toMatchObject({ withinTicks: 1, withinPct: 100 });
  });

  it('drops non-finite and negative measurements rather than clamping them', () => {
    const hist = newBusyHistogram([6]);
    recordBusy(hist, Number.NaN);
    recordBusy(hist, -2);
    recordBusy(hist, Number.POSITIVE_INFINITY);
    // A clamped -2 would become a 0ms tick that fits every budget.
    expect(summarizeBusy(hist).budgets[0]?.withinTicks).toBe(0);
    expect(hist.count).toBe(0);
    // And a dropped measurement must not reserve a worst-tick slot either.
    expect(summarizeBusy(hist).worst).toEqual([]);
  });
});

// The worst-tick list is the histogram's complement: percentiles say what
// the distribution was, this says WHEN to look. It is evidence, so the
// rules that matter are that it is ordered, bounded, and reset per run.
describe('busy worst ticks', () => {
  it('keeps the worst ticks descending, with the moment each started', () => {
    const hist = newBusyHistogram([6]);
    recordBusy(hist, 3, 100);
    recordBusy(hist, 41, 220);
    recordBusy(hist, 12, 340);
    const worst = summarizeBusy(hist).worst;
    expect(worst).toEqual([
      { atMs: 220, busyMs: 41 },
      { atMs: 340, busyMs: 12 },
      { atMs: 100, busyMs: 3 },
    ]);
  });

  it('never grows past the keep bound, whatever order the ticks arrive in', () => {
    const ascending = newBusyHistogram([6]);
    const descending = newBusyHistogram([6]);
    const total = BUSY_WORST_KEEP * 4;
    for (let i = 1; i <= total; i += 1) {
      recordBusy(ascending, i, i * 10);
      recordBusy(descending, total + 1 - i, i * 10);
    }
    for (const hist of [ascending, descending]) {
      const worst = summarizeBusy(hist).worst;
      expect(worst).toHaveLength(BUSY_WORST_KEEP);
      // The K largest values, whichever order they were recorded in.
      expect(worst.map((tick) => tick.busyMs)).toEqual(
        Array.from({ length: BUSY_WORST_KEEP }, (_unused, i) => total - i),
      );
      expect(hist.count).toBe(total);
    }
  });

  it('keeps the earlier tick ahead on a tie', () => {
    const hist = newBusyHistogram([6]);
    recordBusy(hist, 9, 10);
    recordBusy(hist, 9, 20);
    recordBusy(hist, 9, 30);
    expect(summarizeBusy(hist).worst.map((tick) => tick.atMs)).toEqual([10, 20, 30]);
  });

  it('does not let a full list be displaced by a tick that ties its floor', () => {
    const hist = newBusyHistogram([6]);
    for (let i = 0; i < BUSY_WORST_KEEP; i += 1) recordBusy(hist, 5, i);
    recordBusy(hist, 5, 999);
    expect(summarizeBusy(hist).worst.map((tick) => tick.atMs)).toEqual(
      Array.from({ length: BUSY_WORST_KEEP }, (_unused, i) => i),
    );
  });

  it('answers 0 for a tick recorded with no clock rather than NaN', () => {
    const hist = newBusyHistogram([6]);
    recordBusy(hist, 7);
    recordBusy(hist, 6, Number.NaN);
    expect(summarizeBusy(hist).worst).toEqual([
      { atMs: 0, busyMs: 7 },
      { atMs: 0, busyMs: 6 },
    ]);
  });

  // A histogram is per RUN. Nothing carries over, and the check is here
  // rather than in perf.ts because this is where the state lives.
  it('starts empty in a fresh histogram', () => {
    const first = newBusyHistogram([6]);
    recordBusy(first, 50, 1);
    expect(summarizeBusy(first).worst).toHaveLength(1);
    expect(summarizeBusy(newBusyHistogram([6])).worst).toEqual([]);
  });
});

describe('series', () => {
  it('folds count/min/max/mean/last in one pass', () => {
    const series = newSeries();
    for (const value of [10, 30, 20]) recordSeries(series, value);
    expect(series).toMatchObject({ count: 3, min: 10, max: 30, mean: 20, last: 20, sum: 60 });
  });

  it('seeds min and max from the first sample, not from zero', () => {
    const series = newSeries();
    recordSeries(series, 500);
    expect(series.min).toBe(500);
    expect(series.max).toBe(500);
  });
});
