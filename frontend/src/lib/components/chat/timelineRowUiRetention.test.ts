import { describe, expect, it } from 'vitest';
import { makeItem } from '../../../test/helpers/chat';
import type { TimelineNode } from '../../utils/subagentGrouping';
import {
  activeRowUiRetentionSignature,
  collectTimelineRowUiRetention,
  timelineRowUiPruneSignature,
} from './timelineRowUiRetention';

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
        runTailNodeCount: 30,
        isGroupExpanded: () => false,
      },
    );

    expect(new Set(result.itemIds)).toEqual(new Set(['agent-parent']));
    expect([...result.payloads]).toEqual([
      { threadId: 'thread-a', payloadId: 'payload-parent' },
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
        runTailNodeCount: 30,
        isGroupExpanded: () => true,
      },
    );

    expect(new Set(result.itemIds)).toEqual(new Set([
      'agent-parent',
      'agent-child',
    ]));
    expect(new Set([...result.payloads].map((payload) => payload.payloadId))).toEqual(
      new Set(['payload-parent', 'payload-child']),
    );
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
        runTailNodeCount: 30,
        isGroupExpanded: () => false,
      },
    );

    expect(new Set(result.itemIds)).toEqual(new Set([
      'agent-child',
      'visible-row',
    ]));
    expect(new Set(result.groupKeys)).toEqual(new Set(['group:agent-parent']));
  });

  it('changes active signature when an active row completes', () => {
    const active = makeItem({
      id: 'active-row',
      payloadId: 'payload-active',
      threadId: 'thread-a',
      status: 'streaming',
    });
    const completed = { ...active, status: 'completed' as const };

    expect(activeRowUiRetentionSignature([active])).not.toBe(
      activeRowUiRetentionSignature([completed]),
    );
    expect(activeRowUiRetentionSignature([completed])).toBe('');
  });

  // The prune dedupe signature must ignore per-delta summary churn (so
  // streaming text never re-triggers retention collection) while still
  // changing on the inputs retention actually depends on: window
  // position, structure revision, reveal gate, and active-row
  // membership/payload linkage.
  it('prune signature ignores streaming summary churn but sees retention inputs', () => {
    const streaming = makeItem({
      id: 'row-1',
      threadId: 'thread-a',
      status: 'streaming',
      summary: 'partial',
    });
    const base = {
      threadId: 'thread-a',
      timelineRevision: 3,
      revealTurnIndex: 2,
      revealItemIndex: 7,
      nodesLength: 10,
      range: { first: 4, last: 9 },
      items: [streaming],
    };

    const grown = { ...streaming, summary: 'partial plus more streamed text' };
    expect(timelineRowUiPruneSignature({ ...base, items: [grown] })).toBe(
      timelineRowUiPruneSignature(base),
    );

    expect(timelineRowUiPruneSignature({ ...base, range: { first: 5, last: 9 } })).not.toBe(
      timelineRowUiPruneSignature(base),
    );
    expect(timelineRowUiPruneSignature({ ...base, timelineRevision: 4 })).not.toBe(
      timelineRowUiPruneSignature(base),
    );
    const linked = { ...streaming, payloadId: 'payload-1' };
    expect(timelineRowUiPruneSignature({ ...base, items: [linked] })).not.toBe(
      timelineRowUiPruneSignature(base),
    );
    const settled = { ...streaming, status: 'completed' as const };
    expect(timelineRowUiPruneSignature({ ...base, items: [settled] })).not.toBe(
      timelineRowUiPruneSignature(base),
    );
  });
});

function subagentGroup(parent: ReturnType<typeof makeItem>, children: TimelineNode[]): TimelineNode {
  return {
    kind: 'group',
    parent,
    groupKey: `group:${parent.id}`,
    children,
    descendantCount: children.length,
    loadedDescendantCount: children.length,
    latestChildSummary: '',
  };
}
