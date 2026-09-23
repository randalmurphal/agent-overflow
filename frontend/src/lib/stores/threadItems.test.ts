import { applyItemUpsertsToWindow } from './threadItemUpserts';
import { cursorFromItem, itemsWithinLoadedWindow } from './threadItems';
import { describe, expect, it } from 'vitest';
import { makeItem } from '../../test/helpers/chat';
import type { Item } from '../types/models';
import {
  reconcileSnapshotPage,
  compareItemsByTimelinePosition,
  cursorIsValid,
  itemsForThread,
  mergeItemsById,
  mergeMissingItemsById,
  reconcileItemWindow,
} from './threadItems';

type ApplyItemUpsertsOptions = Parameters<typeof applyItemUpsertsToWindow>[0];

function applyWindowUpserts(
  options: Omit<ApplyItemUpsertsOptions, 'newestLoadedTurnIndex' | 'hasMoreNewer'>
    & Partial<Pick<ApplyItemUpsertsOptions, 'newestLoadedTurnIndex' | 'hasMoreNewer'>>,
) {
  return applyItemUpsertsToWindow({
    newestLoadedTurnIndex: null,
    hasMoreNewer: false,
    ...options,
  });
}

describe('cursorIsValid', () => {
  it('accepts head-healed negative item indexes but rejects the empty sentinel', () => {
    expect(cursorIsValid({ turnIndex: 1, itemIndex: -1 })).toBe(true);
    expect(cursorIsValid({ turnIndex: 0, itemIndex: 0 })).toBe(true);
    expect(cursorIsValid({ turnIndex: -1, itemIndex: -1 })).toBe(false);
    expect(cursorIsValid(null)).toBe(false);
    expect(cursorIsValid({ turnIndex: Number.NaN, itemIndex: 0 })).toBe(false);
  });
});

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

  it('preserves current row references when merge rows are equal by value', () => {
    const existing = makeItem({ id: 'existing', summary: 'same' });
    const current = [existing];
    const incoming = makeItem({ id: 'existing', summary: 'same' });

    const merged = mergeItemsById([incoming], current);

    expect(merged).toBe(current);
    expect(merged[0]).toBe(existing);
  });

  it('reconciles full windows without replacing equal row references', () => {
    const existingA = makeItem({ id: 'a', turnIndex: 0, summary: 'same' });
    const existingB = makeItem({ id: 'b', turnIndex: 1, summary: 'same' });
    const current = [existingA, existingB];

    const identical = reconcileItemWindow([
      makeItem({ id: 'a', turnIndex: 0, summary: 'same' }),
      makeItem({ id: 'b', turnIndex: 1, summary: 'same' }),
    ], current);

    expect(identical).toBe(current);
    expect(identical[0]).toBe(existingA);
    expect(identical[1]).toBe(existingB);

    const changedB = makeItem({ id: 'b', turnIndex: 1, summary: 'changed' });
    const changed = reconcileItemWindow([
      makeItem({ id: 'a', turnIndex: 0, summary: 'same' }),
      changedB,
    ], current);

    expect(changed).not.toBe(current);
    expect(changed[0]).toBe(existingA);
    expect(changed[1]).toBe(changedB);
  });

  it('merges a delayed snapshot under newer live rows', () => {
    const live = makeItem({ id: 'streaming', turnIndex: 1, summary: 'live' });
    const liveOnly = makeItem({ id: 'live-only', turnIndex: 2 });
    const missed = makeItem({ id: 'missed', turnIndex: 1, itemIndex: 1 });

    const merged = reconcileSnapshotPage([
      makeItem({ id: 'streaming', turnIndex: 1, summary: 'stale' }),
      missed,
    ], [live, liveOnly], new Set(['streaming', 'live-only']));

    expect(merged.map((item) => item.id)).toEqual(['streaming', 'missed', 'live-only']);
    expect(merged[0]).toBe(live);
    expect(merged[1]).toBe(missed);
    expect(merged[2]).toBe(liveOnly);
  });

  it('keeps the current array when a delayed snapshot contributes nothing', () => {
    const live = makeItem({ id: 'streaming', summary: 'live' });
    const current = [live];
    expect(reconcileSnapshotPage([
      makeItem({ id: 'streaming', summary: 'stale' }),
    ], current, new Set(['streaming']))).toBe(current);
  });

  it('still applies snapshot deletions for untouched rows during live changes', () => {
    const live = makeItem({ id: 'streaming', turnIndex: 1, summary: 'live' });
    const obsolete = makeItem({ id: 'obsolete', turnIndex: 2 });
    const merged = reconcileSnapshotPage([
      makeItem({ id: 'streaming', turnIndex: 1, summary: 'stale' }),
    ], [live, obsolete], new Set(['streaming']));

    expect(merged).toEqual([live]);
  });

  it('does not restore a row removed while the snapshot was in flight', () => {
    const removed = makeItem({ id: 'removed' });
    expect(reconcileSnapshotPage(
      [removed],
      [],
      new Set(),
      new Set(['removed']),
    )).toEqual([]);
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

    const next = applyWindowUpserts({
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

    const next = applyWindowUpserts({
      current,
      incoming: [
        makeItem({ id: 'too-old', threadId: 'thread-1', turnIndex: 1 }),
        corrected,
      ],
      itemIndexById: new Map(current.map((item, index) => [item.id, index])),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: 3,
      hasMoreHistory: true,
    });

    expect(next?.items.map((item) => item.id)).toEqual(['existing']);
    expect(next?.items[0]).toBe(corrected);
  });

  it('accepts head-healed negative-index rows of the oldest loaded turn under the fallback floor', () => {
    // The turn-index fallback floor must sit BELOW every possible index:
    // head-healed prompts persist at negative indexes, and a floor at
    // itemIndex 0 would silently drop their upsert as below-window.
    const current = [
      makeItem({ id: 'response', threadId: 'thread-1', turnIndex: 2, itemIndex: 0 }),
    ];
    const healed = makeItem({
      id: 'healed-prompt',
      threadId: 'thread-1',
      turnIndex: 2,
      itemIndex: -1,
    });

    const next = applyWindowUpserts({
      current,
      incoming: [healed],
      itemIndexById: new Map(current.map((item, index) => [item.id, index])),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: 2,
      hasMoreHistory: true,
    });

    expect(next?.items.map((item) => item.id)).toContain('healed-prompt');
  });

  it('drops new rows below the loaded floor cursor inside the same turn', () => {
    const current = [
      makeItem({ id: 'floor', threadId: 'thread-1', turnIndex: 3, itemIndex: 5 }),
    ];

    const next = applyWindowUpserts({
      current,
      incoming: [
        makeItem({ id: 'same-turn-old', threadId: 'thread-1', turnIndex: 3, itemIndex: 4 }),
        makeItem({ id: 'same-turn-new', threadId: 'thread-1', turnIndex: 3, itemIndex: 6 }),
      ],
      itemIndexById: new Map(current.map((item, index) => [item.id, index])),
      currentThreadId: 'thread-1',
      oldestLoadedCursor: { turnIndex: 3, itemIndex: 5, itemId: 'floor' },
      hasMoreHistory: true,
    });

    expect(next?.items.map((item) => item.id)).toEqual(['floor', 'same-turn-new']);
  });

  it('holds same-turn rows beyond the loaded ceiling when a newer gap exists', () => {
    const current = [
      makeItem({ id: 'ceiling', threadId: 'thread-1', turnIndex: 3, itemIndex: 5 }),
    ];

    const next = applyWindowUpserts({
      current,
      incoming: [
        makeItem({ id: 'same-turn-newer', threadId: 'thread-1', turnIndex: 3, itemIndex: 6 }),
      ],
      itemIndexById: new Map(current.map((item, index) => [item.id, index])),
      currentThreadId: 'thread-1',
      newestLoadedCursor: { turnIndex: 3, itemIndex: 5, itemId: 'ceiling' },
      hasMoreNewer: true,
    });

    expect(next?.items).toBe(current);
    expect(next?.droppedNewerItems).toBe(true);
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

    const next = applyWindowUpserts({
      current,
      incoming: [decided],
      itemIndexById: new Map(current.map((item, index) => [item.id, index])),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: 2,
    });

    expect(next?.items[0]).toBe(decided);
  });

  it('does not flag same-row successful command output chrome as structural', () => {
    const current = [
      makeItem({
        id: 'bash',
        threadId: 'thread-1',
        kind: 'tool_call',
        status: 'running',
        toolName: 'Bash',
        summary: 'Bash: sleep 1',
        meta: JSON.stringify({ input: { command: 'sleep 1' } }),
      }),
    ];
    const completed = {
      ...current[0]!,
      status: 'completed' as const,
      payloadId: 'payload-bash',
      payloadKind: 'command_output',
      payloadMeta: JSON.stringify({ command: 'sleep 1', exitCode: 0 }),
      updatedAt: current[0]!.updatedAt + 1,
    };

    const next = applyWindowUpserts({
      current,
      incoming: [completed],
      itemIndexById: new Map(current.map((item, index) => [item.id, index])),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: 0,
    });

    expect(next?.items[0]).toBe(completed);
    expect(next?.structureChanged).toBe(false);
    // …but the row LEFT the active set, which is a retention change with
    // no structural change in tow. The two flags are independent.
    expect(next?.rowUiRetentionChanged).toBe(true);
  });

  it('flags a rail-exempting payload as structural, and an ordinary one not', () => {
    const current = [
      makeItem({
        id: 'plan',
        threadId: 'thread-1',
        kind: 'tool_call',
        status: 'completed',
        toolName: 'Bash',
        summary: 'Plan',
      }),
    ];
    const apply = (incoming: Item) => applyWindowUpserts({
      current,
      incoming: [incoming],
      itemIndexById: new Map(current.map((item, index) => [item.id, index])),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: 0,
    });

    // A card-style payload takes the row off the activity rail, which decides
    // where the run around it starts and ends — the same class of change as an
    // insertion, and the projection has no other way to hear about it.
    expect(apply({
      ...current[0]!,
      payloadId: 'p-plan',
      payloadKind: 'proposed_plan',
      updatedAt: current[0]!.updatedAt + 1,
    })?.structureChanged).toBe(true);

    // Every other payload kind leaves membership alone, and there are many per
    // turn: treating those as structural would rebuild the timeline per chunk.
    expect(apply({
      ...current[0]!,
      payloadId: 'p-out',
      payloadKind: 'command_output',
      updatedAt: current[0]!.updatedAt + 1,
    })?.structureChanged).toBe(false);
  });

  it('flags task lifecycle completion as structural because it can hide notifications', () => {
    const current = [
      makeItem({
        id: 'task',
        threadId: 'thread-1',
        kind: 'tool_call',
        status: 'running',
        toolName: 'Task',
        meta: JSON.stringify({ task_id: 'task-1' }),
      }),
    ];
    const completed = {
      ...current[0]!,
      status: 'completed' as const,
      updatedAt: current[0]!.updatedAt + 1,
    };

    const next = applyWindowUpserts({
      current,
      incoming: [completed],
      itemIndexById: new Map(current.map((item, index) => [item.id, index])),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: 0,
    });

    expect(next?.structureChanged).toBe(true);
  });

  it('flags wait-carrier metadata changes as structural', () => {
    const current = [
      makeItem({
        id: 'wait',
        threadId: 'thread-1',
        kind: 'tool_call',
        toolName: 'wait_agent',
        meta: JSON.stringify({ input: { tool: 'noop' } }),
      }),
    ];
    const enriched = {
      ...current[0]!,
      meta: JSON.stringify({ input: { tool: 'wait_agent' } }),
      updatedAt: current[0]!.updatedAt + 1,
    };

    const next = applyWindowUpserts({
      current,
      incoming: [enriched],
      itemIndexById: new Map(current.map((item, index) => [item.id, index])),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: 0,
    });

    expect(next?.structureChanged).toBe(true);
  });

  it('does not flag collab-agent status-only chrome as structural', () => {
    const current = [
      makeItem({
        id: 'agent',
        threadId: 'thread-1',
        kind: 'tool_call',
        status: 'running',
        toolName: 'collab_agent',
        meta: JSON.stringify({ input: { tool: 'spawn_agent', receiverThreadIds: ['child-1'] } }),
        payloadMeta: JSON.stringify({ input: { newAgentNickname: 'Reviewer' } }),
      }),
    ];
    const completed = {
      ...current[0]!,
      status: 'completed' as const,
      updatedAt: current[0]!.updatedAt + 1,
    };

    const next = applyWindowUpserts({
      current,
      incoming: [completed],
      itemIndexById: new Map(current.map((item, index) => [item.id, index])),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: 0,
    });

    expect(next?.items[0]).toBe(completed);
    expect(next?.structureChanged).toBe(false);
  });

  it('flags collab-agent receiver metadata changes as structural', () => {
    const current = [
      makeItem({
        id: 'agent',
        threadId: 'thread-1',
        kind: 'tool_call',
        status: 'running',
        toolName: 'collab_agent',
        meta: JSON.stringify({ input: { tool: 'spawn_agent', receiverThreadIds: ['child-1'] } }),
        payloadMeta: JSON.stringify({ input: { newAgentNickname: 'Reviewer' } }),
      }),
    ];
    const renamed = {
      ...current[0]!,
      payloadMeta: JSON.stringify({ input: { newAgentNickname: 'Implementer' } }),
      updatedAt: current[0]!.updatedAt + 1,
    };

    const next = applyWindowUpserts({
      current,
      incoming: [renamed],
      itemIndexById: new Map(current.map((item, index) => [item.id, index])),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: 0,
    });

    expect(next?.structureChanged).toBe(true);
  });

  it('returns the current reference when every incoming upsert is ignored', () => {
    const current = [
      makeItem({ id: 'existing', threadId: 'thread-1', turnIndex: 3 }),
    ];

    expect(applyWindowUpserts({
      current,
      incoming: [
        makeItem({ id: 'foreign', threadId: 'thread-2', turnIndex: 3 }),
        makeItem({ id: 'too-old', threadId: 'thread-1', turnIndex: 1 }),
      ],
      itemIndexById: new Map(current.map((item, index) => [item.id, index])),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: 2,
      hasMoreHistory: true,
    })).toBeNull();
  });
});

// The offscreen row-UI prune proves a no-op from the revision this flag
// drives, so a missed `true` leaks retained expansion state and a
// gratuitous `true` puts the prune's item walk back on the hot path.
describe('applyItemUpsertsToWindow row-UI retention flag', () => {
  const streaming = makeItem({
    id: 'row',
    threadId: 'thread-1',
    turnIndex: 3,
    status: 'streaming',
    summary: 'par',
  });

  function applyOver(current: readonly Item[], incoming: readonly Item[]) {
    return applyWindowUpserts({
      current,
      incoming,
      itemIndexById: new Map(current.map((item, index) => [item.id, index])),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: 0,
    });
  }

  it('stays false for a text-only delta upsert on a streaming row', () => {
    const grown = { ...streaming, summary: 'partial plus more', updatedAt: 5 };
    const next = applyOver([streaming], [grown]);
    expect(next?.items[0]).toBe(grown);
    expect(next?.rowUiRetentionChanged).toBe(false);
  });

  it('flags a payload attaching to a still-streaming row', () => {
    const next = applyOver([streaming], [{ ...streaming, payloadId: 'payload-1', updatedAt: 5 }]);
    expect(next?.rowUiRetentionChanged).toBe(true);
  });

  it('flags a move between the two active statuses', () => {
    const next = applyOver([streaming], [{ ...streaming, status: 'running', updatedAt: 5 }]);
    expect(next?.rowUiRetentionChanged).toBe(true);
  });

  it('flags a settled row going active', () => {
    const settled = { ...streaming, status: 'completed' as const };
    const next = applyOver([settled], [{ ...settled, status: 'running', updatedAt: 5 }]);
    expect(next?.rowUiRetentionChanged).toBe(true);
  });

  it('flags an appended active row and not an appended settled one', () => {
    const active = applyOver([streaming], [
      makeItem({ id: 'appended', threadId: 'thread-1', turnIndex: 4, status: 'running' }),
    ]);
    expect(active?.appendedItems.map((item) => item.id)).toEqual(['appended']);
    expect(active?.rowUiRetentionChanged).toBe(true);

    const settled = applyOver([streaming], [
      makeItem({ id: 'appended', threadId: 'thread-1', turnIndex: 4, status: 'completed' }),
    ]);
    expect(settled?.appendedItems.map((item) => item.id)).toEqual(['appended']);
    expect(settled?.rowUiRetentionChanged).toBe(false);
  });

  it('stays false when one row in a batch moves nothing and another only re-sorts', () => {
    const settledTail = makeItem({
      id: 'tail',
      threadId: 'thread-1',
      turnIndex: 4,
      status: 'completed',
    });
    const next = applyOver([streaming, settledTail], [
      { ...streaming, summary: 'more text', updatedAt: 5 },
      { ...settledTail, turnIndex: 2, updatedAt: 5 },
    ]);
    expect(next?.indexesNeedRebuild).toBe(true);
    expect(next?.rowUiRetentionChanged).toBe(false);
  });

  it('stays false when the only outcome is a dropped newer row', () => {
    const next = applyWindowUpserts({
      current: [streaming],
      incoming: [
        makeItem({ id: 'beyond', threadId: 'thread-1', turnIndex: 9, status: 'running' }),
      ],
      itemIndexById: new Map([['row', 0]]),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: 0,
      newestLoadedTurnIndex: 3,
      hasMoreNewer: true,
    });
    expect(next?.droppedNewerItems).toBe(true);
    expect(next?.rowUiRetentionChanged).toBe(false);
  });
});

describe('applyItemUpsertsToWindow scope admission', () => {
  const anchorRow = (overrides: Partial<Item> = {}) => makeItem({
    id: 'anchor',
    threadId: 'thread-1',
    turnIndex: 4,
    itemIndex: 1,
    kind: 'tool_call',
    toolName: 'Task',
    status: 'running',
    ...overrides,
  });
  const childRow = (overrides: Partial<Item> = {}) => makeItem({
    id: 'child',
    threadId: 'thread-1',
    turnIndex: 4,
    itemIndex: 2,
    parentId: 'anchor',
    ...overrides,
  });
  const indexOf = (items: readonly Item[]) =>
    new Map(items.map((item, index) => [item.id, index]));

  it('never admits a child into the thread window, even with its anchor loaded', () => {
    const current = [anchorRow()];
    expect(applyWindowUpserts({
      current,
      incoming: [childRow(), childRow({ id: 'grandchild', itemIndex: 3, parentId: 'child' })],
      itemIndexById: indexOf(current),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: 4,
    })).toBeNull();
  });

  it('admits the top-level rows of a mixed batch only', () => {
    const next = applyWindowUpserts({
      current: [],
      incoming: [anchorRow(), childRow()],
      itemIndexById: new Map(),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: null,
    });
    expect(next?.items.map((item) => item.id)).toEqual(['anchor']);
    expect(next?.appendedItems.map((item) => item.id)).toEqual(['anchor']);
  });

  it('admits only direct children into a scoped window', () => {
    const next = applyWindowUpserts({
      current: [],
      incoming: [
        anchorRow(),
        childRow(),
        childRow({ id: 'grandchild', itemIndex: 3, parentId: 'child' }),
      ],
      itemIndexById: new Map(),
      currentThreadId: 'thread-1',
      scopeRootId: 'anchor',
      oldestLoadedTurnIndex: null,
    });
    expect(next?.items.map((item) => item.id)).toEqual(['child']);
  });
});

describe('reconcileSnapshotPage', () => {
  it('returns the current reference when nothing moved', () => {
    const rows = [
      makeItem({ id: 'a', threadId: 'thread-1', turnIndex: 1, itemIndex: 0 }),
    ];
    expect(reconcileSnapshotPage(rows, rows, new Set())).toBe(rows);
  });
});

// A row re-persisted without visible change arrives as an upsert that
// differs only in `rev`. Every dedupe keeps the held row (and, on the
// window path, the array reference), but the held row must carry the new
// revision or the next open describes a window the backend no longer has
// (threadWindowDigest.ts). See `adoptRevIfEqual`.
describe('revision carry on absorbed rows', () => {
  it('keeps the window reference and adopts the rev on a revision-only upsert', () => {
    const held = makeItem({ id: 'a', threadId: 'thread-1', rev: 4 });
    const current = [held];

    const next = applyWindowUpserts({
      current,
      incoming: [makeItem({ id: 'a', threadId: 'thread-1', rev: 9 })],
      itemIndexById: new Map([['a', 0]]),
      currentThreadId: 'thread-1',
    });

    expect(next).toBeNull();
    expect(current[0]).toBe(held);
    expect(held.rev).toBe(9);
  });

  it('adopts the rev of an equal-by-value paging row', () => {
    const held = makeItem({ id: 'a', summary: 'same', rev: 4 });
    const current = [held];

    expect(mergeItemsById([makeItem({ id: 'a', summary: 'same', rev: 9 })], current)).toBe(current);
    expect(held.rev).toBe(9);
  });

  it('adopts the rev of an equal-by-value reconciled row', () => {
    const held = makeItem({ id: 'a', summary: 'same', rev: 4 });
    const current = [held];

    const next = reconcileItemWindow([makeItem({ id: 'a', summary: 'same', rev: 9 })], current);

    expect(next).toBe(current);
    expect(held.rev).toBe(9);
  });

  it('still replaces the row when anything rendered changed', () => {
    const held = makeItem({ id: 'a', summary: 'before', rev: 4 });
    const incoming = makeItem({ id: 'a', summary: 'after', rev: 9 });

    const next = reconcileItemWindow([incoming], [held]);

    expect(next[0]).toBe(incoming);
    expect(held.rev).toBe(4);
  });
});

// A pushed top-level row can land inside the coordinate range of a run
// the pane holds only part of (docs/architecture/timeline-window-pages.md
// §6). Inserting it would make the run's loaded span discontiguous, which
// every count on its record is stated against.
describe('applyItemUpsertsToWindow activity-run routing', () => {
  const current = [
    makeItem({ id: 'p0', turnIndex: 0, itemIndex: 0 }),
    makeItem({ id: 'b', turnIndex: 0, itemIndex: 4, kind: 'tool_call' }),
  ];
  const itemIndexById = new Map(current.map((item, index) => [item.id, index]));
  const base = {
    current,
    itemIndexById,
    currentThreadId: 'thread-1',
    oldestLoadedCursor: { turnIndex: 0, itemIndex: 0 },
    newestLoadedCursor: { turnIndex: 0, itemIndex: 4 },
    hasMoreNewer: false,
  };

  it('refuses a row inside a held run and reports the run instead', () => {
    const incoming = [makeItem({ id: 'new', turnIndex: 0, itemIndex: 2, kind: 'tool_call' })];
    const next = applyWindowUpserts({
      ...base,
      incoming,
      runCoveringUnshipped: (item) => (item.id === 'new' ? 'run-a' : null),
    })!;
    expect(next.items.map((item) => item.id)).toEqual(['p0', 'b']);
    expect(next.appendedItems).toEqual([]);
    expect(next.structureChanged).toBe(false);
    expect(next.dirtiedRunKeys).toEqual(['run-a']);
  });

  it('reports each run once for a burst', () => {
    const incoming = [
      makeItem({ id: 'n1', turnIndex: 0, itemIndex: 2, kind: 'tool_call' }),
      makeItem({ id: 'n2', turnIndex: 0, itemIndex: 3, kind: 'tool_call' }),
    ];
    const next = applyWindowUpserts({ ...base, incoming, runCoveringUnshipped: () => 'run-a' })!;
    expect(next.dirtiedRunKeys).toEqual(['run-a']);
  });

  it('appends a row no run claims, and updates a loaded row', () => {
    const incoming = [
      makeItem({ id: 'tail', turnIndex: 0, itemIndex: 5, kind: 'tool_call' }),
      makeItem({ id: 'b', turnIndex: 0, itemIndex: 4, kind: 'tool_call', status: 'errored' }),
    ];
    const next = applyWindowUpserts({ ...base, incoming, runCoveringUnshipped: () => null })!;
    expect(next.items.map((item) => item.id)).toEqual(['p0', 'b', 'tail']);
    expect(next.dirtiedRunKeys).toEqual([]);
  });

  it('never routes a subagent child', () => {
    const claimed: string[] = [];
    const incoming = [
      makeItem({ id: 'child', turnIndex: 0, itemIndex: 2, parentId: 'b' }),
    ];
    applyWindowUpserts({
      ...base,
      incoming,
      runCoveringUnshipped: (item) => {
        claimed.push(item.id);
        return 'run-a';
      },
    });
    expect(claimed).toEqual([]);
  });
});

describe('itemsWithinLoadedWindow', () => {
  const row = (id: string, turnIndex: number, itemIndex = 0): Item =>
    makeItem({ id, turnIndex, itemIndex });

  it('returns the same array when every row sits inside the edges', () => {
    const items = [row('a', 3), row('b', 4)];
    expect(itemsWithinLoadedWindow(items, cursorFromItem(items[0]), cursorFromItem(items[1]))).toBe(items);
    expect(itemsWithinLoadedWindow(items, null, null)).toBe(items);
  });

  it('hides rows outside the edges', () => {
    const items = [row('island', 0), row('a', 3), row('b', 4), row('late', 9)];
    const visible = itemsWithinLoadedWindow(items, cursorFromItem(items[1]), cursorFromItem(items[2]));
    expect(visible.map((item) => item.id)).toEqual(['a', 'b']);
  });
});
