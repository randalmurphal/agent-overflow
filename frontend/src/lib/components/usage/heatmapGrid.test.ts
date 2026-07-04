import { describe, expect, it } from 'vitest';
import { buildHeatmapGrid, computeQuartiles, type UsageDayBucket } from './heatmapGrid';

// Fixed reference "now": Friday, July 3 2026 (matches this session's
// current date). Its week runs Mon 2026-06-29 .. Sun 2026-07-05, so
// Sat 07-04 / Sun 07-05 are future relative to "today".
const NOW = new Date(2026, 6, 3, 15, 30, 0).getTime();

function bucket(dateKey: string, overrides: Partial<UsageDayBucket> = {}): UsageDayBucket {
  return {
    bucket: dateKey,
    costUsd: 0,
    inputTokens: 0,
    outputTokens: 0,
    unpricedRows: 0,
    ...overrides,
  };
}

describe('computeQuartiles', () => {
  it('returns zeros for an empty input', () => {
    expect(computeQuartiles([])).toEqual([0, 0, 0]);
  });

  it('returns the single value for all three quartiles when there is one value', () => {
    expect(computeQuartiles([5])).toEqual([5, 5, 5]);
  });

  it('computes linear-interpolation quartiles over 1..10', () => {
    const values = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10];
    const [q1, q2, q3] = computeQuartiles(values);
    expect(q1).toBeCloseTo(3.25, 5);
    expect(q2).toBeCloseTo(5.5, 5);
    expect(q3).toBeCloseTo(7.75, 5);
  });

  it('is order-independent', () => {
    const sorted = computeQuartiles([1, 2, 3, 4, 5]);
    const shuffled = computeQuartiles([4, 1, 5, 2, 3]);
    expect(shuffled).toEqual(sorted);
  });
});

describe('buildHeatmapGrid', () => {
  it('date bucketing: maps a bucket onto its exact local date key', () => {
    const columns = buildHeatmapGrid(
      [bucket('2026-07-03', { costUsd: 4.2 })],
      NOW,
      1,
    );
    const cell = columns[0].cells.find((c) => c.dateKey === '2026-07-03');
    expect(cell).toBeDefined();
    expect(cell?.costUsd).toBe(4.2);
    expect(cell?.hasData).toBe(true);
  });

  it('a date with no matching bucket has no data and sits at level 0', () => {
    const columns = buildHeatmapGrid([bucket('2026-07-03', { costUsd: 9 })], NOW, 1);
    const empty = columns[0].cells.find((c) => c.dateKey === '2026-07-01');
    expect(empty?.hasData).toBe(false);
    expect(empty?.level).toBe(0);
  });

  it('week alignment: every column starts on a Monday and runs Mon..Sun in weekday order', () => {
    const columns = buildHeatmapGrid([], NOW, 4);
    for (const column of columns) {
      const monday = new Date(`${column.weekStartKey}T00:00:00`);
      expect(monday.getDay()).toBe(1);
      expect(column.cells).toHaveLength(7);
      column.cells.forEach((cell, i) => {
        expect(cell.weekday).toBe(i);
        const cellDate = new Date(`${cell.dateKey}T00:00:00`);
        expect(cellDate.getDay()).toBe((i + 1) % 7); // Mon=1..Sat=6,Sun=0
      });
    }
  });

  it('week alignment: the last column is the week containing "now"', () => {
    const columns = buildHeatmapGrid([], NOW, 1);
    expect(columns).toHaveLength(1);
    expect(columns[0].weekStartKey).toBe('2026-06-29');
    expect(columns[0].cells.map((c) => c.dateKey)).toEqual([
      '2026-06-29', '2026-06-30', '2026-07-01', '2026-07-02',
      '2026-07-03', '2026-07-04', '2026-07-05',
    ]);
  });

  it('week alignment: grid spans exactly the requested number of weeks', () => {
    const columns = buildHeatmapGrid([], NOW, 26);
    expect(columns).toHaveLength(26);
    // Oldest column is 25 weeks before the current week's Monday.
    expect(columns[0].weekStartKey).toBe('2026-01-05');
  });

  it('marks dates after "now"\'s local day as future', () => {
    const columns = buildHeatmapGrid([], NOW, 1);
    const byDate = new Map(columns[0].cells.map((c) => [c.dateKey, c]));
    expect(byDate.get('2026-07-03')?.isFuture).toBe(false); // today
    expect(byDate.get('2026-07-02')?.isFuture).toBe(false); // past
    expect(byDate.get('2026-07-04')?.isFuture).toBe(true); // future
    expect(byDate.get('2026-07-05')?.isFuture).toBe(true); // future
  });

  it('quartile stepping: quantizes cost into 5 steps using only in-window non-zero values', () => {
    // 4 non-zero days across a 1-week window: 1, 2, 3, 4.
    // Q1=1.75, Q2=2.5, Q3=3.25 (linear interpolation over [1,2,3,4]).
    const buckets = [
      bucket('2026-06-29', { costUsd: 1 }),
      bucket('2026-06-30', { costUsd: 2 }),
      bucket('2026-07-01', { costUsd: 3 }),
      bucket('2026-07-02', { costUsd: 4 }),
    ];
    const columns = buildHeatmapGrid(buckets, NOW, 1);
    const byDate = new Map(columns[0].cells.map((c) => [c.dateKey, c]));
    expect(byDate.get('2026-06-29')?.level).toBe(1); // 1 <= 1.75
    expect(byDate.get('2026-06-30')?.level).toBe(2); // 1.75 < 2 <= 2.5
    expect(byDate.get('2026-07-01')?.level).toBe(3); // 2.5 < 3 <= 3.25
    expect(byDate.get('2026-07-02')?.level).toBe(4); // 4 > 3.25
    expect(byDate.get('2026-07-03')?.level).toBe(0); // no data
  });

  it('quartile stepping: falls back to token totals when every bucket has zero cost', () => {
    const buckets = [
      bucket('2026-06-29', { costUsd: 0, inputTokens: 100, outputTokens: 0 }),
      bucket('2026-06-30', { costUsd: 0, inputTokens: 900, outputTokens: 100 }),
    ];
    const columns = buildHeatmapGrid(buckets, NOW, 1);
    const byDate = new Map(columns[0].cells.map((c) => [c.dateKey, c]));
    // Both days have nonzero tokens, so both should quantize above level 0
    // even though costUsd is 0 for both.
    expect(byDate.get('2026-06-29')?.level).toBeGreaterThan(0);
    expect(byDate.get('2026-06-30')?.level).toBeGreaterThan(0);
    // The higher-token day should not be quantized below the lower one.
    const lowLevel = byDate.get('2026-06-29')!.level;
    const highLevel = byDate.get('2026-06-30')!.level;
    expect(highLevel).toBeGreaterThanOrEqual(lowLevel);
  });

  it('does not fall back to tokens when any cost is present', () => {
    const buckets = [
      bucket('2026-06-29', { costUsd: 0, inputTokens: 5_000_000, outputTokens: 0 }),
      bucket('2026-06-30', { costUsd: 0.5, inputTokens: 0, outputTokens: 0 }),
    ];
    const columns = buildHeatmapGrid(buckets, NOW, 1);
    const byDate = new Map(columns[0].cells.map((c) => [c.dateKey, c]));
    // Total cost across the buckets is > 0, so cost (not tokens) drives
    // quantization: the huge-token/zero-cost day sits at level 0.
    expect(byDate.get('2026-06-29')?.level).toBe(0);
    expect(byDate.get('2026-06-30')?.level).toBeGreaterThan(0);
  });

  it('month label: appears only on the column containing the 1st of a month', () => {
    const columns = buildHeatmapGrid([], NOW, 6);
    const labeled = columns.filter((c) => c.monthLabel !== null);
    // Window spans late May through early July 2026 -> June 1 and July 1
    // each fall in exactly one column.
    expect(labeled.map((c) => c.monthLabel)).toEqual(['Jun', 'Jul']);
    for (const column of labeled) {
      expect(column.cells.some((cell) => cell.dayOfMonth === 1)).toBe(true);
    }
  });

  it('month label: columns without a 1st-of-month day are unlabeled', () => {
    const columns = buildHeatmapGrid([], NOW, 6);
    // Week of Jun 8-14 2026 contains no 1st-of-month day.
    const midJune = columns.find((c) => c.weekStartKey === '2026-06-08');
    expect(midJune?.monthLabel).toBeNull();
  });
});
