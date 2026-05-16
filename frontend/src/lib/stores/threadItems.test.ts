import { describe, expect, it } from 'vitest';
import { makeItem } from '../../test/helpers/chat';
import {
  applyItemUpsertsToWindow,
  compareItemsByTimelinePosition,
  itemsForThread,
  mergeItemsById,
  mergeMissingItemsById,
} from './threadItems';

describe('threadItems', () => {
  it('sorts by turn index and item index', () => {
    const sorted = [
      makeItem({ id: 'b', turnIndex: 1, itemIndex: 2 }),
      makeItem({ id: 'a', turnIndex: 0, itemIndex: 9 }),
      makeItem({ id: 'c', turnIndex: 1, itemIndex: 1 }),
    ].sort(compareItemsByTimelinePosition);

    expect(sorted.map((item) => item.id)).toEqual(['a', 'c', 'b']);
  });

  it('filters loaded rows to the active thread', () => {
    expect(itemsForThread([
      makeItem({ id: 'keep', threadId: 'thread-1' }),
      makeItem({ id: 'drop', threadId: 'thread-2' }),
    ], 'thread-1').map((item) => item.id)).toEqual(['keep']);
    expect(itemsForThread(null, 'thread-1')).toEqual([]);
  });

  it('merges paging rows by id and replaces duplicate rows with incoming copies', () => {
    const currentAncestor = makeItem({
      id: 'ancestor',
      turnIndex: 0,
      summary: 'summary-only',
    });
    const enrichedAncestor = makeItem({
      id: 'ancestor',
      turnIndex: 0,
      summary: 'enriched',
      payloadId: 'payload-ancestor',
    });
    const current = [
      currentAncestor,
      makeItem({ id: 'child', turnIndex: 5 }),
    ];

    const merged = mergeItemsById([
      enrichedAncestor,
      makeItem({ id: 'between', turnIndex: 3 }),
    ], current);

    expect(merged.map((item) => item.id)).toEqual(['ancestor', 'between', 'child']);
    expect(merged[0]).toBe(enrichedAncestor);
    expect(merged.filter((item) => item.id === 'ancestor')).toHaveLength(1);
  });

  it('returns the current reference when merge rows are already present by reference', () => {
    const existing = makeItem({ id: 'existing' });
    const current = [existing];

    expect(mergeItemsById([existing], current)).toBe(current);
    expect(mergeMissingItemsById([existing], current)).toBe(current);
  });

  it('adds only missing rows and preserves existing row references', () => {
    const streamed = makeItem({ id: 'streamed', turnIndex: 1, summary: 'live' });
    const staleStreamed = makeItem({ id: 'streamed', turnIndex: 1, summary: 'stale' });

    const merged = mergeMissingItemsById([
      makeItem({ id: 'load', turnIndex: 0 }),
      staleStreamed,
    ], [streamed]);

    expect(merged.map((item) => item.id)).toEqual(['load', 'streamed']);
    expect(merged[1]).toBe(streamed);
    expect(merged[1].summary).toBe('live');
  });

  it('adds only the first missing row for a duplicated incoming id', () => {
    const first = makeItem({ id: 'duplicate', turnIndex: 1, summary: 'first' });
    const second = makeItem({ id: 'duplicate', turnIndex: 1, summary: 'second' });

    const merged = mergeMissingItemsById([first, second], []);

    expect(merged).toEqual([first]);
  });

  it('applies in-thread upserts and keeps the timeline sorted', () => {
    const current = [
      makeItem({ id: 'first', threadId: 'thread-1', turnIndex: 2 }),
      makeItem({ id: 'last', threadId: 'thread-1', turnIndex: 4 }),
    ];
    const replacement = makeItem({
      id: 'last',
      threadId: 'thread-1',
      turnIndex: 1,
      summary: 'moved earlier',
    });

    const next = applyItemUpsertsToWindow({
      current,
      incoming: [
        makeItem({ id: 'foreign', threadId: 'thread-2', turnIndex: 0 }),
        replacement,
        makeItem({ id: 'middle', threadId: 'thread-1', turnIndex: 3 }),
      ],
      itemIndexById: new Map(current.map((item, index) => [item.id, index])),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: 2,
    });

    expect(next?.items.map((item) => item.id)).toEqual(['last', 'first', 'middle']);
    expect(next?.items[0]).toBe(replacement);
    expect(next?.indexesNeedRebuild).toBe(true);
  });

  it('drops new rows below the loaded floor but still accepts existing-row corrections', () => {
    const current = [
      makeItem({ id: 'existing', threadId: 'thread-1', turnIndex: 3, summary: 'old' }),
    ];
    const corrected = makeItem({
      id: 'existing',
      threadId: 'thread-1',
      turnIndex: 1,
      summary: 'corrected below floor',
    });

    const next = applyItemUpsertsToWindow({
      current,
      incoming: [
        makeItem({ id: 'too-old', threadId: 'thread-1', turnIndex: 1 }),
        corrected,
      ],
      itemIndexById: new Map(current.map((item, index) => [item.id, index])),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: 3,
    });

    expect(next?.items.map((item) => item.id)).toEqual(['existing']);
    expect(next?.items[0]).toBe(corrected);
  });

  it('applies existing-row upserts when only an observable optional field changes', () => {
    const current = [
      makeItem({
        id: 'approval',
        threadId: 'thread-1',
        turnIndex: 3,
        decision: '',
      }),
    ];
    const decided = {
      ...current[0]!,
      decision: 'approved' as const,
    };

    const next = applyItemUpsertsToWindow({
      current,
      incoming: [decided],
      itemIndexById: new Map(current.map((item, index) => [item.id, index])),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: 2,
    });

    expect(next?.items[0]).toBe(decided);
  });

  it('returns the current reference when every incoming upsert is ignored', () => {
    const current = [
      makeItem({ id: 'existing', threadId: 'thread-1', turnIndex: 3 }),
    ];

    expect(applyItemUpsertsToWindow({
      current,
      incoming: [
        makeItem({ id: 'foreign', threadId: 'thread-2', turnIndex: 3 }),
        makeItem({ id: 'too-old', threadId: 'thread-1', turnIndex: 1 }),
      ],
      itemIndexById: new Map(current.map((item, index) => [item.id, index])),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: 2,
    })).toBeNull();
  });
});
