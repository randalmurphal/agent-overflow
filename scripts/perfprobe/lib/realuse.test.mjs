import test from 'node:test';
import assert from 'node:assert/strict';
import {
  HISTOGRAM_BUCKET_COUNT,
  estimateMissedRefreshes,
  histogramCount,
  histogramCountAbove,
  histogramPercentile,
  mergeSparseHistograms,
  metricDelta,
  parseNumericCsv,
  processCpuDelta,
} from './realuse.mjs';

test('histogram helpers merge windows without averaging percentiles', () => {
  const buckets = mergeSparseHistograms([
    [[24, 90], [48, 10]],
    [[24, 90], [72, 10]],
  ]);
  assert.equal(buckets.length, HISTOGRAM_BUCKET_COUNT);
  assert.equal(histogramCount(buckets), 200);
  assert.equal(histogramPercentile(buckets, 0.5), 6);
  assert.equal(histogramPercentile(buckets, 0.95), 12);
  assert.equal(histogramPercentile(buckets, 1), 18);
  assert.equal(histogramCountAbove(buckets, 12), 10);
  assert.equal(estimateMissedRefreshes(buckets, 6), 30);
});

test('CPU deltas keep process replacement visible instead of inventing its history', () => {
  const result = processCpuDelta(
    [
      { id: 1, type: 'browser', cpuTime: 10 },
      { id: 2, type: 'renderer', cpuTime: 20 },
      { id: 3, type: 'GPU', cpuTime: 30 },
    ],
    [
      { id: 1, type: 'browser', cpuTime: 10.4 },
      { id: 2, type: 'renderer', cpuTime: 21.2 },
      { id: 4, type: 'network.mojom.NetworkService', cpuTime: 0.5 },
    ],
    10_000,
    8,
  );
  assert.equal(result.processCount, 3);
  assert.equal(result.newProcesses, 1);
  assert.equal(result.disappearedProcesses, 1);
  assert.ok(Math.abs(result.rawPercent - 16) < 1e-9);
  assert.ok(Math.abs(result.normalizedPercent - 2) < 1e-9);
  assert.ok(Math.abs(result.byGroupPercent.browser - 0.5) < 1e-9);
  assert.ok(Math.abs(result.byGroupPercent.renderer - 1.5) < 1e-9);
});

test('metric deltas reject resets and absent counters', () => {
  assert.equal(metricDelta({ TaskDuration: 2 }, { TaskDuration: 2.25 }, 'TaskDuration', 1000), 250);
  assert.equal(metricDelta({ TaskDuration: 2 }, { TaskDuration: 1 }, 'TaskDuration', 1000), null);
  assert.equal(metricDelta({}, {}, 'TaskDuration', 1000), null);
});

test('numeric CSV parsing accepts one header across appended sessions', () => {
  const text = [
    'utc,elapsedMs,processCount,censusMissingCount',
    '2026-08-29T00:00:00Z,1000,6,0',
    'utc,elapsedMs,processCount,censusMissingCount',
    '2026-08-29T01:00:00Z,1000,7,1',
  ].join('\n');
  assert.deepEqual(parseNumericCsv(text), [
    { utc: '2026-08-29T00:00:00Z', elapsedMs: 1000, processCount: 6, censusMissingCount: 0 },
    { utc: '2026-08-29T01:00:00Z', elapsedMs: 1000, processCount: 7, censusMissingCount: 1 },
  ]);
});

test('numeric CSV parsing fails on a partial write', () => {
  assert.throws(
    () => parseNumericCsv('utc,elapsedMs,processCount\n2026-08-29T00:00:00Z,1000'),
    /has 2 fields, want 3/,
  );
});
