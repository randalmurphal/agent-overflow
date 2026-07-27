import { describe, it, expect } from 'vitest';
import type { TimelineNode } from './subagentGrouping';
import { nodeSignature } from './timelineStructureSignature';
import { makeItem } from '../../test/helpers/chat';

function leaf(overrides = {}): TimelineNode {
  return { kind: 'leaf', item: makeItem(overrides) };
}

describe('nodeSignature', () => {
  it('is reproducible for an unchanged leaf', () => {
    // This is the property the earlier `pane.timelineRevision` key lacked:
    // a monotonic counter is different on every revisit, so the cached size
    // never matched. The signature must be identical for the same row so a
    // revisited settled row replays.
    const a = leaf({ id: 'a', summary: 'hi', status: 'completed', updatedAt: 1 });
    const b = leaf({ id: 'a', summary: 'hi', status: 'completed', updatedAt: 1 });
    expect(nodeSignature(a)).toBe(nodeSignature(b));
  });

  it('encodes each leaf content-height input', () => {
    const base = nodeSignature(leaf({ id: 'a', summary: 'hi', status: 'completed', updatedAt: 1 }));
    // summary length (text height)
    expect(nodeSignature(leaf({ id: 'a', summary: 'hello', status: 'completed', updatedAt: 1 }))).not.toBe(base);
    // status (streaming/spinner vs settled)
    expect(nodeSignature(leaf({ id: 'a', summary: 'hi', status: 'streaming', updatedAt: 1 }))).not.toBe(base);
    // updatedAt (Go bumps on every streaming append)
    expect(nodeSignature(leaf({ id: 'a', summary: 'hi', status: 'completed', updatedAt: 2 }))).not.toBe(base);
    // id (row identity)
    expect(nodeSignature(leaf({ id: 'z', summary: 'hi', status: 'completed', updatedAt: 1 }))).not.toBe(base);
  });

  it('signs an activity run by its rendered shape, and liveness is not one', () => {
    // A stale prior replaying across a shape change is the failure this whole
    // signature exists to prevent. A run has TWO shapes, because its header is
    // unconditional: that header alone, or the header over a capped clip. One
    // letter would let a closed run replay the height it had while open.
    const activityRun = (over: { collapsed: boolean; live: boolean }): TimelineNode => ({
      kind: 'activity_run',
      runId: 'r1',
      threadId: 'thread-1',
      children: [leaf({ id: 'a' })],
      mountedFrom: 0,
      mountedRows: 1,
      memberItemIds: ['a'],
      ...over,
    });

    const closed = nodeSignature(activityRun({ collapsed: true, live: false }));
    const open = nodeSignature(activityRun({ collapsed: false, live: false }));
    expect(closed).not.toBe(open);

    // And exactly two over the whole 2x2: `collapsed` already has liveness
    // folded into it (`ActivityRunIdentity.collapsedFor` resolves it once per
    // pass), so signing on `live` as well would drop a perfectly good prior
    // every time a later run took the tail — the run's height did not change.
    expect(nodeSignature(activityRun({ collapsed: true, live: true }))).toBe(closed);
    expect(nodeSignature(activityRun({ collapsed: false, live: true }))).toBe(open);
  });

  it('signs group nodes by key and member count', () => {
    const group = (members: number): TimelineNode => ({
      kind: 'read_group',
      groupKey: 'reads:item-1',
      threadId: 'thread-1',
      members: Array.from({ length: members }, (_, i) => makeItem({ id: `read-${i}` })),
    });
    const two = nodeSignature(group(2));
    expect(nodeSignature(group(2))).toBe(two); // reproducible
    expect(nodeSignature(group(3))).not.toBe(two); // membership grew → taller card
  });
});
