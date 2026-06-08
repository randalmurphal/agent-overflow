import { describe, expect, it } from 'vitest';
import { makeItem } from '../../test/helpers/chat';
import {
  applyItemUpsertsToWindow,
  compareItemsByTimelinePosition,
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
