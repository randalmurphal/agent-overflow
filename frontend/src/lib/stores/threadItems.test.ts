import { describe, expect, it } from 'vitest';
import { makeItem } from '../../test/helpers/chat';
import type { Item } from '../types/models';
import {
  applyItemUpsertsToWindow,
  applySyncPage,
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

describe('applyItemUpsertsToWindow parented admission', () => {
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

  it('lands a child whose anchor is loaded', () => {
    const current = [anchorRow()];
    const next = applyWindowUpserts({
      current,
      incoming: [childRow()],
      itemIndexById: indexOf(current),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: 4,
    });
    expect(next?.items.map((item) => item.id)).toEqual(['anchor', 'child']);
    expect(next?.rejectedParentedItems).toEqual([]);
  });

  it('lands a child whose anchor arrives earlier in the same batch', () => {
    const next = applyWindowUpserts({
      current: [],
      incoming: [anchorRow(), childRow()],
      itemIndexById: new Map(),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: null,
    });
    expect(next?.items.map((item) => item.id)).toEqual(['anchor', 'child']);
    expect(next?.rejectedParentedItems).toEqual([]);
  });

  it('refuses a new child whose anchor is nowhere', () => {
    const next = applyWindowUpserts({
      current: [],
      incoming: [childRow()],
      itemIndexById: new Map(),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: null,
    });
    expect(next?.items).toEqual([]);
    expect(next?.rejectedParentedItems.map((item) => item.id)).toEqual(['child']);
    expect(next?.structureChanged).toBe(false);
  });

  it('refuses grandchildren transitively when the top anchor is missing', () => {
    const mid = childRow({ id: 'mid', kind: 'tool_call', toolName: 'Task' });
    const leaf = makeItem({
      id: 'leaf',
      threadId: 'thread-1',
      turnIndex: 4,
      itemIndex: 3,
      parentId: 'mid',
    });
    const next = applyWindowUpserts({
      current: [],
      incoming: [mid, leaf],
      itemIndexById: new Map(),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: null,
    });
    expect(next?.items).toEqual([]);
    expect(next?.rejectedParentedItems.map((item) => item.id)).toEqual(['mid', 'leaf']);
  });

  it('refuses a child whose same-batch anchor fell below the floor', () => {
    // The disagreement a pre-merge filter could not see: the anchor is
    // refused by the floor guard, so the child it would have vouched for
    // must be refused too instead of landing as an unreachable orphan.
    const current = [
      makeItem({ id: 'tail', threadId: 'thread-1', turnIndex: 6, itemIndex: 0 }),
    ];
    const next = applyWindowUpserts({
      current,
      incoming: [
        anchorRow({ turnIndex: 5, itemIndex: 1 }),
        childRow({ turnIndex: 5, itemIndex: 9 }),
      ],
      itemIndexById: indexOf(current),
      currentThreadId: 'thread-1',
      oldestLoadedCursor: { turnIndex: 5, itemIndex: 3 },
      oldestLoadedTurnIndex: 5,
      hasMoreHistory: true,
    });
    expect(next?.items.map((item) => item.id)).toEqual(['tail']);
    expect(next?.rejectedParentedItems.map((item) => item.id)).toEqual(['child']);
  });

  it('updates a loaded child regardless of its parentage', () => {
    // Hydration installs children whose anchor may later prune away; an
    // update to a row the pane renders always applies.
    const current = [childRow()];
    const next = applyWindowUpserts({
      current,
      incoming: [childRow({ summary: 'progress' })],
      itemIndexById: indexOf(current),
      currentThreadId: 'thread-1',
      oldestLoadedTurnIndex: 4,
    });
    expect(next?.changedItems.map((item) => item.id)).toEqual(['child']);
    expect(next?.rejectedParentedItems).toEqual([]);
  });
});

describe('applySyncPage subagent admission', () => {
  it('keeps live children whose anchor survives and reports the orphaned ones', () => {
    const anchor = makeItem({
      id: 'anchor',
      threadId: 'thread-1',
      turnIndex: 2,
      itemIndex: 0,
      kind: 'tool_call',
      toolName: 'Task',
      status: 'running',
    });
    const child = makeItem({
      id: 'child',
      threadId: 'thread-1',
      turnIndex: 2,
      itemIndex: 1,
      parentId: 'anchor',
    });
    const stray = makeItem({
      id: 'stray',
      threadId: 'thread-1',
      turnIndex: 2,
      itemIndex: 5,
      parentId: 'gone',
    });
    const page = [
      makeItem({ id: 'top', threadId: 'thread-1', turnIndex: 1, itemIndex: 0 }),
    ];
    const result = applySyncPage(
      page,
      [anchor, child, stray],
      new Set(['anchor', 'child', 'stray']),
    );
    expect(result.items.map((item) => item.id)).toEqual(['top', 'anchor', 'child']);
    expect(result.orphanedLiveChildren.map((item) => item.id)).toEqual(['stray']);
  });

  it('drops a live child transitively when its parent does not survive', () => {
    const anchor = makeItem({
      id: 'anchor',
      threadId: 'thread-1',
      turnIndex: 2,
      itemIndex: 0,
      kind: 'tool_call',
      toolName: 'Task',
    });
    const child = makeItem({
      id: 'child',
      threadId: 'thread-1',
      turnIndex: 2,
      itemIndex: 1,
      parentId: 'anchor',
    });
    // The anchor is neither in the page nor live-touched, so it is a
    // paint-only row and drops; its live child must not outlive it.
    const result = applySyncPage([], [anchor, child], new Set(['child']));
    expect(result.items).toEqual([]);
    expect(result.orphanedLiveChildren.map((item) => item.id)).toEqual(['child']);
  });

  it('returns the current reference when nothing moved', () => {
    const rows = [
      makeItem({ id: 'a', threadId: 'thread-1', turnIndex: 1, itemIndex: 0 }),
    ];
    const result = applySyncPage(rows, rows, new Set());
    expect(result.items).toBe(rows);
    expect(result.orphanedLiveChildren).toEqual([]);
  });
});
