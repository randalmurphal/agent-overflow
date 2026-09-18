import { describe, it, expect } from 'vitest';
import { wholeRunNodeFields } from '../../test/helpers/activityRuns';
import type { TimelineNode } from './subagentGrouping';
import { CLOSED_ACTIVITY_RUN_SIGNATURE, nodeSignature as signNode } from './timelineStructureSignature';
import { makeItem } from '../../test/helpers/chat';

function leaf(overrides = {}): TimelineNode {
  return { kind: 'leaf', item: makeItem(overrides) };
}

// Store-row resolver that always defers to the node: the steady state
// after a structural pass, where every node's `item` IS the store row.
function nodeSignature(node: TimelineNode): string {
  return signNode(node, () => undefined);
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

  it('signs whether a run has a boundary row on each edge, not what it counts', () => {
    // A boundary is one row tall for any count, and the count moves on stub
    // refreshes that change no geometry: signing the number would drop the
    // prior on every refresh, signing presence keeps it exactly as long as
    // the row's shape holds.
    const run = (over: { unshippedBefore?: number; unshippedAfter?: number; mountedFrom?: number }): TimelineNode => ({
      kind: 'activity_run',
      runId: 'r1',
      threadId: 'thread-1',
      children: ['a', 'b', 'c'].map((id) => leaf({ id })),
      mountedFrom: over.mountedFrom ?? 0,
      mountedRows: 3 - (over.mountedFrom ?? 0),
      membershipEpoch: 1,
      memberItemIds: ['a', 'b', 'c'],
      summaryItemIds: ['a', 'b', 'c'],
      collapsed: false,
      live: false,
      atTail: false,
      ...wholeRunNodeFields(['a', 'b', 'c']),
      unshippedBefore: over.unshippedBefore ?? 0,
      unshippedAfter: over.unshippedAfter ?? 0,
      memberCount: 3 + (over.unshippedBefore ?? 0) + (over.unshippedAfter ?? 0),
    });
    const whole = nodeSignature(run({}));
    expect(nodeSignature(run({ unshippedBefore: 4 }))).not.toBe(whole);
    expect(nodeSignature(run({ unshippedAfter: 4 }))).not.toBe(whole);
    expect(nodeSignature(run({ unshippedBefore: 4 }))).toBe(nodeSignature(run({ unshippedBefore: 40 })));
    // A loaded row hidden above the window is the same boundary row.
    expect(nodeSignature(run({ mountedFrom: 1 }))).not.toBe(whole);
  });

  it('signs an activity run by its rendered shape, and liveness is not one', () => {
    // A stale prior replaying across a shape change is the failure this whole
    // signature exists to prevent. A run has TWO shapes, because its header is
    // unconditional: that header alone, or the header over a capped clip. One
    // letter would let a closed run replay the height it had while open.
    const activityRun = (over: {
      collapsed: boolean;
      live: boolean;
      atTail?: boolean;
    }): TimelineNode => ({
      kind: 'activity_run',
      runId: 'r1',
      threadId: 'thread-1',
      children: [leaf({ id: 'a' })],
      mountedFrom: 0,
      mountedRows: 1,
      membershipEpoch: 1,
      memberItemIds: ['a'],
      summaryItemIds: ['a'],
      ...wholeRunNodeFields(['a']),
      atTail: over.atTail ?? over.live,
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
    // `atTail` is not signed either, and independently of `live` — the pair
    // diverges exactly when closing prose waits behind the reveal gate, and
    // that moment must not drop the run's measured prior.
    expect(nodeSignature(activityRun({ collapsed: false, live: false, atTail: true }))).toBe(open);
  });

  it('signs an activity run by its first member, not its registry-minted run id', () => {
    // Run ids are minted per pane registry and re-minted on every mount, so a
    // signature keyed on them could never replay a prior across a reopen.
    const run = (over: { runId: string; members: string[] }): TimelineNode => ({
      kind: 'activity_run',
      runId: over.runId,
      threadId: 'thread-1',
      children: over.members.map((id) => leaf({ id })),
      mountedFrom: 0,
      mountedRows: over.members.length,
      membershipEpoch: 1,
      memberItemIds: over.members,
      summaryItemIds: over.members,
      ...wholeRunNodeFields(over.members),
      collapsed: false,
      live: false,
      atTail: false,
    });

    const first = nodeSignature(run({ runId: 'r1', members: ['a', 'b'] }));
    expect(nodeSignature(run({ runId: 'r9', members: ['a', 'b'] }))).toBe(first);
    expect(nodeSignature(run({ runId: 'r1', members: ['z', 'b'] }))).not.toBe(first);
    expect(nodeSignature(run({ runId: 'r1', members: ['a'] }))).not.toBe(first);
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
  it('signs a leaf by the store\'s current row, not the node\'s minted item', () => {
    // The projection re-mints leaf nodes only on a structural change, while
    // the store replaces the Item on every write. After a streaming row
    // settles the node still carries the streaming-era Item and the row on
    // screen has the settled height; the signature must follow the store.
    const streaming = makeItem({ id: 'a', summary: '', status: 'streaming', updatedAt: 1 });
    const settled = makeItem({ id: 'a', summary: 'a settled answer', status: 'completed', updatedAt: 2 });
    const staleNode: TimelineNode = { kind: 'leaf', item: streaming };
    const freshNode: TimelineNode = { kind: 'leaf', item: settled };
    const store = new Map([['a', settled]]);
    expect(signNode(staleNode, (id) => store.get(id))).toBe(nodeSignature(freshNode));
    expect(signNode(staleNode, (id) => store.get(id))).not.toBe(nodeSignature(staleNode));
    // A row the store no longer holds (pruned, evicted) signs by the node's item.
    expect(signNode(staleNode, () => undefined)).toBe(nodeSignature(staleNode));
  });

  it('memoizes a leaf per resolved item; a content write replaces the item and the signature follows', () => {
    // The memo's invalidation IS item identity: the store never mutates a
    // signed field in place (`adoptRevIfEqual` adopts only `rev`), so the
    // same Item object always signs the same string, and a write hands the
    // resolver a new object. Distinct-but-equal items staying equal is
    // pinned above.
    const item = makeItem({ id: 'a', summary: 'hi', status: 'streaming', updatedAt: 1 });
    const node: TimelineNode = { kind: 'leaf', item };
    const first = nodeSignature(node);
    expect(nodeSignature(node)).toBe(first);
    const grown = makeItem({ id: 'a', summary: 'hi there', status: 'streaming', updatedAt: 2 });
    const store = new Map([['a', grown]]);
    expect(signNode(node, (id) => store.get(id))).not.toBe(first);
    expect(signNode(node, (id) => store.get(id))).toBe(nodeSignature({ kind: 'leaf', item: grown }));
  });

  it('signs every closed activity run with one shared shape key', () => {
    // A closed run is its one-line header alone, so its height is a property
    // of the geometry bucket, not of the run. One shared key is what lets a
    // run captured OPEN (the tail run at a switch-away after a turn) resolve
    // on the reopen that renders it closed, from any closed run the bucket
    // measured before.
    const run = (over: { members: string[]; collapsed: boolean; mountedRows?: number }): TimelineNode => ({
      kind: 'activity_run',
      runId: 'r1',
      threadId: 'thread-1',
      children: over.members.map((id) => leaf({ id })),
      mountedFrom: 0,
      mountedRows: over.mountedRows ?? over.members.length,
      membershipEpoch: 1,
      memberItemIds: over.members,
      summaryItemIds: over.members,
      ...wholeRunNodeFields(over.members),
      collapsed: over.collapsed,
      live: false,
      atTail: false,
    });
    expect(nodeSignature(run({ members: ['a'], collapsed: true }))).toBe(CLOSED_ACTIVITY_RUN_SIGNATURE);
    expect(nodeSignature(run({ members: ['x', 'y', 'z'], collapsed: true, mountedRows: 2 }))).toBe(
      CLOSED_ACTIVITY_RUN_SIGNATURE,
    );
    // Open runs keep their identity, membership and mount window.
    expect(nodeSignature(run({ members: ['a'], collapsed: false }))).not.toBe(
      nodeSignature(run({ members: ['x', 'y', 'z'], collapsed: false })),
    );
  });
});
