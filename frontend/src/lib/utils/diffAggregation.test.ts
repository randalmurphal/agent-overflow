import { describe, expect, it, vi } from 'vitest';
import type { Item, PayloadMeta } from '../types/models';
import {
  aggregateAgentDiffs,
  selectAgentDiffEntries,
  summarizeEntries,
  type AgentDiffEntry,
} from './diffAggregation';

function item(overrides: Partial<Item>): Item {
  return {
    id: 'i-1',
    threadId: 't-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'message',
    role: 'assistant',
    summary: '',
    createdAt: 0,
    ...overrides,
  };
}

describe('selectAgentDiffEntries', () => {
  it('returns an empty list when no items are provided', () => {
    expect(selectAgentDiffEntries([])).toEqual([]);
  });

  it('returns an empty list when no items are diffs', () => {
    const items = [
      item({ id: 'a', kind: 'message' }),
      item({ id: 'b', kind: 'command_execution', payloadId: 'p1' }),
    ];
    expect(selectAgentDiffEntries(items)).toEqual([]);
  });

  it('picks a single diff and returns the payload metadata', () => {
    const items = [item({ id: 'd1', kind: 'diff', payloadId: 'p1' })];
    expect(selectAgentDiffEntries(items)).toEqual([
      { itemId: 'd1', payloadId: 'p1', turnIndex: 0, itemIndex: 0 },
    ]);
  });

  it('preserves the order of diff items', () => {
    const items = [
      item({ id: 'd2', kind: 'diff', payloadId: 'p2', turnIndex: 0, itemIndex: 2 }),
      item({ id: 'd1', kind: 'diff', payloadId: 'p1', turnIndex: 0, itemIndex: 0 }),
      item({ id: 'd3', kind: 'diff', payloadId: 'p3', turnIndex: 1, itemIndex: 0 }),
    ];
    expect(selectAgentDiffEntries(items).map((e) => e.itemId)).toEqual(['d2', 'd1', 'd3']);
  });

  it('ignores diffs without a payloadId', () => {
    const items = [
      item({ id: 'd1', kind: 'diff' }),
      item({ id: 'd2', kind: 'diff', payloadId: 'p2' }),
    ];
    expect(selectAgentDiffEntries(items).map((e) => e.itemId)).toEqual(['d2']);
  });

  it('skips non-diff items interleaved with diffs', () => {
    const items = [
      item({ id: 'u', role: 'user', kind: 'message' }),
      item({ id: 'd1', kind: 'diff', payloadId: 'p1', itemIndex: 1 }),
      item({ id: 'a', kind: 'message', itemIndex: 2 }),
      item({ id: 'd2', kind: 'diff', payloadId: 'p2', itemIndex: 3 }),
      item({ id: 'c', kind: 'command_execution', itemIndex: 4 }),
    ];
    expect(selectAgentDiffEntries(items).map((e) => e.payloadId)).toEqual(['p1', 'p2']);
  });
});

describe('aggregateAgentDiffs', () => {
  function entry(payloadId: string, turnIndex = 0, itemIndex = 0): AgentDiffEntry {
    return { itemId: `i-${payloadId}`, payloadId, turnIndex, itemIndex };
  }

  it('returns an empty string when no entries are given', async () => {
    const cache = new Map<string, string>();
    const load = vi.fn();
    expect(await aggregateAgentDiffs([], load, cache)).toBe('');
    expect(load).not.toHaveBeenCalled();
  });

  it('calls the loader once per payload and concatenates results', async () => {
    const cache = new Map<string, string>();
    const load = vi.fn(async (id: string) => `diff-${id}`);
    const result = await aggregateAgentDiffs(
      [entry('p1'), entry('p2'), entry('p3')],
      load,
      cache,
    );
    expect(result).toBe('diff-p1\n\ndiff-p2\n\ndiff-p3');
    expect(load).toHaveBeenCalledTimes(3);
  });

  it('memoizes through the cache so repeat entries hit once', async () => {
    const cache = new Map<string, string>();
    const load = vi.fn(async (id: string) => `d-${id}`);

    await aggregateAgentDiffs([entry('p1'), entry('p1'), entry('p2')], load, cache);
    expect(load).toHaveBeenCalledTimes(2); // p1 fetched once, p2 fetched once
    expect(cache.get('p1')).toBe('d-p1');
    expect(cache.get('p2')).toBe('d-p2');
  });

  it('uses cached values on subsequent invocations without reloading', async () => {
    const cache = new Map<string, string>([['p1', 'cached-p1']]);
    const load = vi.fn(async (id: string) => `fresh-${id}`);

    const result = await aggregateAgentDiffs([entry('p1'), entry('p2')], load, cache);
    expect(result).toBe('cached-p1\n\nfresh-p2');
    expect(load).toHaveBeenCalledTimes(1);
    expect(load).toHaveBeenCalledWith('p2');
  });

  it('propagates loader errors so callers can surface them', async () => {
    const cache = new Map<string, string>();
    const load = vi.fn(async () => {
      throw new Error('load failed');
    });
    await expect(
      aggregateAgentDiffs([entry('p1')], load, cache),
    ).rejects.toThrow('load failed');
  });
});

describe('summarizeEntries', () => {
  function metaRec(id: string, ins: number, del: number): PayloadMeta {
    return {
      id,
      kind: 'diff',
      meta: JSON.stringify({ insertions: ins, deletions: del, filePath: 'x' }),
      createdAt: 0,
    };
  }

  it('returns zeros for an empty entry list', () => {
    expect(summarizeEntries([], new Map())).toEqual({
      insertions: 0,
      deletions: 0,
      fileCount: 0,
    });
  });

  it('sums insertions / deletions / file counts across entries', () => {
    const metas = new Map<string, PayloadMeta>([
      ['p1', metaRec('p1', 3, 1)],
      ['p2', metaRec('p2', 10, 5)],
      ['p3', metaRec('p3', 0, 7)],
    ]);
    expect(
      summarizeEntries(
        [
          { itemId: 'a', payloadId: 'p1', turnIndex: 0, itemIndex: 0 },
          { itemId: 'b', payloadId: 'p2', turnIndex: 0, itemIndex: 1 },
          { itemId: 'c', payloadId: 'p3', turnIndex: 0, itemIndex: 2 },
        ],
        metas,
      ),
    ).toEqual({ insertions: 13, deletions: 13, fileCount: 3 });
  });

  it('treats entries without matching metas as zero-contribution', () => {
    const metas = new Map<string, PayloadMeta>([['p1', metaRec('p1', 5, 2)]]);
    expect(
      summarizeEntries(
        [
          { itemId: 'a', payloadId: 'p1', turnIndex: 0, itemIndex: 0 },
          { itemId: 'b', payloadId: 'missing', turnIndex: 0, itemIndex: 1 },
        ],
        metas,
      ),
    ).toEqual({ insertions: 5, deletions: 2, fileCount: 1 });
  });

  it('skips invalid meta JSON rather than throwing', () => {
    const metas = new Map<string, PayloadMeta>([
      ['p1', { id: 'p1', kind: 'diff', meta: '{not json}', createdAt: 0 }],
      ['p2', metaRec('p2', 2, 3)],
    ]);
    expect(
      summarizeEntries(
        [
          { itemId: 'a', payloadId: 'p1', turnIndex: 0, itemIndex: 0 },
          { itemId: 'b', payloadId: 'p2', turnIndex: 0, itemIndex: 1 },
        ],
        metas,
      ),
    ).toEqual({ insertions: 2, deletions: 3, fileCount: 1 });
  });
});
