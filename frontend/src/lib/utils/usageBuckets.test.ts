import { expect, it } from 'vitest';
import { UsageBucket } from '../stores/bindings';
import { combineUsageBuckets } from './usageBuckets';

it('combines every numeric counter by group, keeps unpriced totals explicit, and does not mutate inputs', () => {
  const first = new UsageBucket({ bucket: '', inputTokens: 1, outputTokens: 2, cacheReadInputTokens: 3,
    cacheCreationInputTokens: 4, reasoningOutputTokens: 5, costUsd: 6, turnCount: 7, sessionCount: 8,
    unpricedRows: 9, costSource: 'provider-estimate' });
  const other = new UsageBucket({ ...first, costSource: '' });
  const totals = combineUsageBuckets([first, other, new UsageBucket({ bucket: 'other', outputTokens: 20 })]);
  expect(totals).toEqual([new UsageBucket({ bucket: '', inputTokens: 2, outputTokens: 4, cacheReadInputTokens: 6,
    cacheCreationInputTokens: 8, reasoningOutputTokens: 10, costUsd: 12, turnCount: 14, sessionCount: 16,
    unpricedRows: 18, costSource: '' }), new UsageBucket({ bucket: 'other', outputTokens: 20 })]);
  expect(first.costUsd).toBe(6);
  expect(first.costSource).toBe('provider-estimate');
});
