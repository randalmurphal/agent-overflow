import { describe, expect, it } from 'vitest';
import {
  FRAME_BUCKET_CEILING_MS,
  framePercentile,
  newFrameHistogram,
  newSeries,
  recordFrame,
  recordSeries,
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
    // The overflow bucket answers with the exact max, which is the only
    // value in it we actually know.
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
