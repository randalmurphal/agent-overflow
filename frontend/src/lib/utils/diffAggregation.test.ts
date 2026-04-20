import { describe, expect, it, vi } from 'vitest';
import {
  aggregateAgentDiffs,
  selectAgentDiffEntries,
  summarizeEntries,
} from './diffAggregation';
import { makeItem } from '../../test/helpers/chat';

describe('selectAgentDiffEntries', () => {
  it('selects raw diff payloads and exact tool-result patches', () => {
    const items = [
      makeItem({
        id: 'diff-1',
        payloadId: 'p-diff',
        payloadKind: 'diff',
        payloadMeta: JSON.stringify({ insertions: 2, deletions: 1 }),
      }),
      makeItem({
        id: 'tool-1',
        payloadId: 'p-tool',
        payloadKind: 'tool_result',
        payloadMeta: JSON.stringify({
          inlineDiff: { availability: 'exact_patch', files: [{ path: 'a.ts', insertions: 4, deletions: 0 }] },
        }),
      }),
      makeItem({
        id: 'tool-2',
        payloadId: 'p-summary',
        payloadKind: 'tool_result',
        payloadMeta: JSON.stringify({
          inlineDiff: { availability: 'summary_only', files: [{ path: 'b.ts', insertions: 1, deletions: 1 }] },
        }),
      }),
    ];

    expect(selectAgentDiffEntries(items).map((entry) => entry.payloadId)).toEqual(['p-diff', 'p-tool']);
  });
});

describe('aggregateAgentDiffs', () => {
  it('loads each payload once and concatenates in entry order', async () => {
    const load = vi.fn(async (payloadId: string) => `diff:${payloadId}`);
    const cache = new Map<string, string>();

    const text = await aggregateAgentDiffs([
      { itemId: 'a', payloadId: 'p1', turnIndex: 0, itemIndex: 0 },
      { itemId: 'b', payloadId: 'p2', turnIndex: 0, itemIndex: 1 },
      { itemId: 'c', payloadId: 'p1', turnIndex: 1, itemIndex: 0 },
    ], load, cache);

    expect(text).toBe('diff:p1\n\ndiff:p2\n\ndiff:p1');
    expect(load).toHaveBeenCalledTimes(2);
  });

  it('returns an empty string for no entries', async () => {
    expect(await aggregateAgentDiffs([], vi.fn(), new Map())).toBe('');
  });
});

describe('summarizeEntries', () => {
  it('aggregates diff payload metadata', () => {
    const items = [
      makeItem({
        id: 'diff-1',
        payloadId: 'p1',
        payloadKind: 'diff',
        payloadMeta: JSON.stringify({ insertions: 3, deletions: 1 }),
      }),
      makeItem({
        id: 'tool-1',
        payloadId: 'p2',
        payloadKind: 'tool_result',
        payloadMeta: JSON.stringify({
          inlineDiff: {
            files: [
              { path: 'a.ts', insertions: 5, deletions: 2 },
              { path: 'b.ts', insertions: 1, deletions: 0 },
            ],
          },
        }),
      }),
    ];

    expect(summarizeEntries([
      { itemId: 'diff-1', payloadId: 'p1', turnIndex: 0, itemIndex: 0 },
      { itemId: 'tool-1', payloadId: 'p2', turnIndex: 0, itemIndex: 1 },
    ], items)).toEqual({
      insertions: 9,
      deletions: 3,
      fileCount: 3,
    });
  });

  it('skips malformed metadata', () => {
    const items = [
      makeItem({
        id: 'bad',
        payloadId: 'p1',
        payloadKind: 'diff',
        payloadMeta: '{not json}',
      }),
    ];

    expect(summarizeEntries([
      { itemId: 'bad', payloadId: 'p1', turnIndex: 0, itemIndex: 0 },
    ], items)).toEqual({
      insertions: 0,
      deletions: 0,
      fileCount: 0,
    });
  });
});
