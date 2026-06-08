import { describe, expect, it } from 'vitest';
import { makeItem } from '../../../test/helpers/chat';
import type { TimelineNode } from '../../utils/subagentGrouping';
import {
  activeRowUiRetentionSignature,
  collectTimelineRowUiRetention,
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
        isGroupExpanded: () => false,
      },
    );

    expect(new Set(result.retention.itemIds)).toEqual(new Set(['agent-parent']));
    expect([...result.retention.payloads]).toEqual([
      { threadId: 'thread-a', payloadId: 'payload-parent' },
    ]);
    expect(new Set(result.retention.groupKeys)).toEqual(new Set(['group:agent-parent']));
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
      },
    );

    expect(new Set(result.retention.itemIds)).toEqual(new Set([
      'agent-parent',
      'agent-child',
    ]));
    expect(new Set([...result.retention.payloads].map((payload) => payload.payloadId))).toEqual(
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
        isGroupExpanded: () => false,
      },
    );

    expect(new Set(result.retention.itemIds)).toEqual(new Set([
      'agent-child',
      'visible-row',
    ]));
    expect(new Set(result.retention.groupKeys)).toEqual(new Set(['group:agent-parent']));
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
});

function subagentGroup(parent: ReturnType<typeof makeItem>, children: TimelineNode[]): TimelineNode {
  return {
    kind: 'group',
    parent,
    groupKey: `group:${parent.id}`,
    children,
    descendantCount: children.length,
    latestChildSummary: '',
  };
}
