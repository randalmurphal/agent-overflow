import { describe, it, expect } from 'vitest';
import type { TimelineNode } from './subagentGrouping';
import { timelineStructureSignature } from './timelineStructureSignature';
import { makeItem } from '../../test/helpers/chat';

function leaf(overrides = {}): TimelineNode {
  return { kind: 'leaf', item: makeItem(overrides) };
}

describe('timelineStructureSignature', () => {
  it('is reproducible for the same node sequence', () => {
    // This is the property the earlier `pane.timelineRevision` key lacked:
    // a monotonic counter is different on every revisit, so the cached sizes
    // never matched. The signature must be identical for the same rows so a
    // revisited settled thread replays.
    const a = [leaf({ id: 'a' }), leaf({ id: 'b' })];
    const b = [leaf({ id: 'a' }), leaf({ id: 'b' })];
    expect(timelineStructureSignature(a)).toBe(timelineStructureSignature(b));
  });

  it('encodes each leaf content-height input', () => {
    const base = timelineStructureSignature([leaf({ id: 'a', summary: 'hi', status: 'completed', updatedAt: 1 })]);
    // summary length (text height)
    expect(timelineStructureSignature([leaf({ id: 'a', summary: 'hello', status: 'completed', updatedAt: 1 })])).not.toBe(base);
    // status (streaming/spinner vs settled)
    expect(timelineStructureSignature([leaf({ id: 'a', summary: 'hi', status: 'streaming', updatedAt: 1 })])).not.toBe(base);
    // updatedAt (Go bumps on every streaming append)
    expect(timelineStructureSignature([leaf({ id: 'a', summary: 'hi', status: 'completed', updatedAt: 2 })])).not.toBe(base);
    // id (structure)
    expect(timelineStructureSignature([leaf({ id: 'z', summary: 'hi', status: 'completed', updatedAt: 1 })])).not.toBe(base);
  });

  it('changes on row order, count, and removal', () => {
    const ab = timelineStructureSignature([leaf({ id: 'a' }), leaf({ id: 'b' })]);
    expect(timelineStructureSignature([leaf({ id: 'b' }), leaf({ id: 'a' })])).not.toBe(ab); // reorder
    expect(timelineStructureSignature([leaf({ id: 'a' }), leaf({ id: 'b' }), leaf({ id: 'c' })])).not.toBe(ab); // added
    expect(timelineStructureSignature([leaf({ id: 'a' })])).not.toBe(ab); // removed
  });

  it('signs group nodes by key and member count', () => {
    const group = (members: number): TimelineNode => ({
      kind: 'read_group',
      groupKey: 'reads:item-1',
      threadId: 'thread-1',
      members: Array.from({ length: members }, (_, i) => makeItem({ id: `read-${i}` })),
    });
    const two = timelineStructureSignature([group(2)]);
    expect(timelineStructureSignature([group(2)])).toBe(two); // reproducible
    expect(timelineStructureSignature([group(3)])).not.toBe(two); // membership grew → taller card
  });

  it('does not collide distinct sequences that share substrings', () => {
    // The `\n` join is the collision guard: a single leaf whose summary length
    // happens to render `:`-separated digits must not equal a two-node
    // sequence. (ids/status/numbers contain no newline.)
    const one = timelineStructureSignature([leaf({ id: 'a:b', summary: 'x' })]);
    const two = timelineStructureSignature([leaf({ id: 'a' }), leaf({ id: 'b' })]);
    expect(one).not.toBe(two);
  });

  it('returns empty string for an empty timeline', () => {
    expect(timelineStructureSignature([])).toBe('');
  });
});
