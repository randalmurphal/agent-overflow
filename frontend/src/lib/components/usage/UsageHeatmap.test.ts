import { beforeEach, describe, expect, it } from 'vitest';
import { flushSync } from 'svelte';
import { render, waitFor } from '@testing-library/svelte';
import UsageHeatmap from './UsageHeatmap.svelte';
import { heatmapWindowStartMs } from './heatmapGrid';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { UsageBucket, type UsageQuery } from '../../stores/bindings';
import { bumpUsageRefresh, resetUsageRefreshForTest } from '../../stores/usageRefresh.svelte';

function dateKey(d: Date): string {
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

function dayBucket(key: string, costUsd: number): UsageBucket {
  return new UsageBucket({
    bucket: key,
    inputTokens: 0,
    outputTokens: 100,
    cacheReadInputTokens: 0,
    cacheCreationInputTokens: 0,
    reasoningOutputTokens: 0,
    costUsd,
    turnCount: 1,
    sessionCount: 1,
    unpricedRows: 0,
  });
}

describe('<UsageHeatmap>', () => {
  let queries: UsageQuery[] = [];
  let responses: UsageBucket[][] = [];

  beforeEach(() => {
    resetUsageRefreshForTest();
    queries = [];
    responses = [];
    setBindingMock('GetUsageStats', async (query: UsageQuery) => {
      queries.push(query);
      return responses[Math.min(queries.length, responses.length) - 1] ?? [];
    });
  });

  it('keeps painted cells through a usage refresh and applies the new totals', async () => {
    const today = new Date();
    const yesterday = new Date(today);
    yesterday.setDate(today.getDate() - 1);
    const todayKey = dateKey(today);
    const yesterdayKey = dateKey(yesterday);
    responses = [[dayBucket(todayKey, 2)], [dayBucket(todayKey, 3), dayBucket(yesterdayKey, 1)]];

    const { container } = render(UsageHeatmap, { props: { provider: '', projectId: '' } });
    const cell = (key: string) => container.querySelector(`[data-testid="usage-heatmap-cell"][data-date="${key}"]`);
    await waitFor(() => expect(cell(todayKey)?.getAttribute('data-level')).not.toBe('0'));
    expect(cell(yesterdayKey)?.getAttribute('data-level')).toBe('0');

    bumpUsageRefresh('any-thread');
    flushSync();
    // The refresh must not clear the grid while the new report is in flight.
    expect(cell(todayKey)?.getAttribute('data-level')).not.toBe('0');

    await waitFor(() => expect(cell(yesterdayKey)?.getAttribute('data-level')).not.toBe('0'));
    expect(queries).toHaveLength(2);
    // The window is the grid's first local midnight, identical across refreshes.
    expect(queries.map((q) => q.fromMillis)).toEqual([
      heatmapWindowStartMs(Date.now()),
      heatmapWindowStartMs(Date.now()),
    ]);
  });
});
