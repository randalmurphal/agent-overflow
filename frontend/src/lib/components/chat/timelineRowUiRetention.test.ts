import { describe, expect, it } from 'vitest';
import { makeItem } from '../../../test/helpers/chat';
import type { Item } from '../../types/models';
import type { TimelineNode } from '../../utils/subagentGrouping';
import {
  collectTimelineRowUiRetention,
  timelineRowUiPruneSignature,
} from './timelineRowUiRetention';
import { payloadRetentionKey } from '../../utils/rowUiRetention';

/** The pane's `getItemById`, over the rows a case says are loaded. */
function resolveFrom(items: readonly Item[]): (itemId: string) => Item | undefined {
  const byId = new Map(items.map((item) => [item.id, item]));
  return (itemId) => byId.get(itemId);
}

describe('timeline row UI retention', () => {
  it('retains collapsed subagent group state without retaining hidden child rows', () => {
    const parent = makeItem({
      id: 'agent-parent',
      payloadId: 'payload-parent',
      threadId: 'thread-a',
    });
    const hiddenChild = makeItem({
      id: 'agent-child',
      payloadId: 'payload-child',
      threadId: 'thread-a',
    });
    const group = subagentGroup(parent, [{ kind: 'leaf', item: hiddenChild }]);

    const result = collectTimelineRowUiRetention(
      [group],
      [parent, hiddenChild],
      { first: 0, last: 0 },
      {
        nodeBuffer: 0,
        tailNodeCount: 0,
        isGroupExpanded: () => false,
        resolveItem: resolveFrom([parent, hiddenChild]),
      },
    );

    expect(new Set(result.itemIds)).toEqual(new Set(['agent-parent']));
    expect([...result.payloads]).toEqual([
      payloadRetentionKey('thread-a', 'payload-parent'),
    ]);
    expect(new Set(result.groupKeys)).toEqual(new Set(['group:agent-parent']));
  });

  it('retains expanded subagent descendants', () => {
    const parent = makeItem({
      id: 'agent-parent',
      payloadId: 'payload-parent',
      threadId: 'thread-a',
    });
    const child = makeItem({
      id: 'agent-child',
      payloadId: 'payload-child',
      threadId: 'thread-a',
    });
    const group = subagentGroup(parent, [{ kind: 'leaf', item: child }]);

    const result = collectTimelineRowUiRetention(
      [group],
      [parent, child],
      { first: 0, last: 0 },
      {
        nodeBuffer: 0,
        tailNodeCount: 0,
        isGroupExpanded: () => true,
        resolveItem: resolveFrom([parent, child]),
      },
    );

    expect(new Set(result.itemIds)).toEqual(new Set([
      'agent-parent',
      'agent-child',
    ]));
    expect(new Set(result.payloads)).toEqual(new Set([
      payloadRetentionKey('thread-a', 'payload-parent'),
      payloadRetentionKey('thread-a', 'payload-child'),
    ]));
  });

  it('retains group keys for active offscreen descendants', () => {
    const parent = makeItem({
      id: 'agent-parent',
      payloadId: 'payload-parent',
      threadId: 'thread-a',
    });
    const activeChild = makeItem({
      id: 'agent-child',
      payloadId: 'payload-child',
      threadId: 'thread-a',
      status: 'running',
    });
    const visible = makeItem({
      id: 'visible-row',
      payloadId: 'payload-visible',
      threadId: 'thread-a',
    });
    const group = subagentGroup(parent, [{ kind: 'leaf', item: activeChild }]);

    const result = collectTimelineRowUiRetention(
      [group, { kind: 'leaf', item: visible }],
      [parent, activeChild, visible],
      { first: 1, last: 1 },
      {
        nodeBuffer: 0,
        tailNodeCount: 0,
        isGroupExpanded: () => false,
        resolveItem: resolveFrom([parent, activeChild, visible]),
      },
    );

    expect(new Set(result.itemIds)).toEqual(new Set([
      'agent-child',
      'visible-row',
    ]));
    expect(new Set(result.groupKeys)).toEqual(new Set(['group:agent-parent']));
  });

  // Nodes are rebuilt on STRUCTURE only, so the item refs they carry are
  // frozen at the last structural pass. A payload id that arrives after it
  // (a tool completing, its output landing in a payload) would otherwise be
  // retained under the ref's key — releasing the key the mounted row is
  // actually holding, which collapses the reader's expansion on remount.
  it('retains the CURRENT payload of a row whose node ref is stale', () => {
    const stale = makeItem({
      id: 'tool:1',
      threadId: 'thread-a',
      payloadId: '',
      status: 'running',
    });
    const current = { ...stale, payloadId: 'payload-landed', status: 'completed' as const };

    const result = collectTimelineRowUiRetention(
      [{ kind: 'leaf', item: stale }],
      [current],
      { first: 0, last: 0 },
      {
        nodeBuffer: 0,
        tailNodeCount: 0,
        isGroupExpanded: () => false,
        resolveItem: resolveFrom([current]),
      },
    );

    expect(new Set(result.itemIds)).toEqual(new Set(['tool:1']));
    expect([...result.payloads]).toEqual([
      payloadRetentionKey('thread-a', 'payload-landed'),
    ]);
  });

  it('retains a group parent and read-group members by their current rows', () => {
    const staleParent = makeItem({ id: 'agent-parent', threadId: 'thread-a', payloadId: '' });
    const currentParent = { ...staleParent, payloadId: 'payload-parent' };
    const staleRead = makeItem({ id: 'read:1', threadId: 'thread-a', payloadId: 'payload-old' });
    const currentRead = { ...staleRead, payloadId: 'payload-new' };
    const group = subagentGroup(staleParent, []);

    const result = collectTimelineRowUiRetention(
      [group, { kind: 'read_group', members: [staleRead], groupKey: 'read:group' } as TimelineNode],
      [currentParent, currentRead],
      { first: 0, last: 1 },
      {
        nodeBuffer: 0,
        tailNodeCount: 0,
        isGroupExpanded: () => false,
        resolveItem: resolveFrom([currentParent, currentRead]),
      },
    );

    expect(new Set(result.payloads)).toEqual(new Set([
      payloadRetentionKey('thread-a', 'payload-parent'),
      payloadRetentionKey('thread-a', 'payload-new'),
    ]));
  });

  // A row that has left the store — folded into a subagent aggregate, or
  // pruned — has no current answer, and the node's own snapshot is the last
  // thing known about it. Retaining that keeps the reader's expansion alive
  // across a fold/unfold round trip; the state is disposed when the row
  // stops appearing in the node band at all.
  it('falls back to the node snapshot for a row the store no longer holds', () => {
    const evicted = makeItem({
      id: 'agent-child',
      threadId: 'thread-a',
      payloadId: 'payload-evicted',
    });

    const result = collectTimelineRowUiRetention(
      [{ kind: 'leaf', item: evicted }],
      [],
      { first: 0, last: 0 },
      {
        nodeBuffer: 0,
        tailNodeCount: 0,
        isGroupExpanded: () => false,
        resolveItem: () => undefined,
      },
    );

    expect(new Set(result.itemIds)).toEqual(new Set(['agent-child']));
    expect([...result.payloads]).toEqual([
      payloadRetentionKey('thread-a', 'payload-evicted'),
    ]);
  });

  // Every dedupe input is a scalar — nothing here may reach for the item
  // list, which is the whole point of the store-maintained retention
  // revision (see utils/rowUiRetention.ts). Active-row membership moving
  // is proven at write time and arrives here as that revision.
  it('prune signature covers every retention input as a scalar', () => {
    const base = {
      threadId: 'thread-a',
      timelineRevision: 3,
      revealTurnIndex: 2,
      revealItemIndex: 7,
      nodesLength: 10,
      activityRunRevision: 0,
      range: { first: 4, last: 9 },
      rowUiRetentionRevision: 11,
    };

    expect(timelineRowUiPruneSignature({ ...base })).toBe(
      timelineRowUiPruneSignature(base),
    );

    const moved: Array<Partial<typeof base>> = [
      { threadId: 'thread-b' },
      { timelineRevision: 4 },
      { revealTurnIndex: 3 },
      { revealItemIndex: 8 },
      { nodesLength: 11 },
      // A run's mount window moves without touching structure, node count, or
      // range: same everything, different mounted children. Without this input
      // the pass would bail as a no-op and keep retaining the window the reader
      // just left.
      { activityRunRevision: 1 },
      { range: { first: 5, last: 9 } },
      { rowUiRetentionRevision: 12 },
    ];
    for (const override of moved) {
      expect(timelineRowUiPruneSignature({ ...base, ...override })).not.toBe(
        timelineRowUiPruneSignature(base),
      );
    }
  });
});

function subagentGroup(parent: ReturnType<typeof makeItem>, children: TimelineNode[]): TimelineNode {
  return {
    kind: 'group',
    parent,
    anchor: parent,
    groupKey: `group:${parent.id}`,
    children,
    descendantCount: children.length,
    loadedDescendantCount: children.length,
    latestChildSummary: '',
  };
}
