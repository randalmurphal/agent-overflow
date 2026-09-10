import { describe, expect, it } from 'vitest';
import { makeItem } from '../../test/helpers/chat';
import type { Item } from '../types/models';
import { userMessageIdentity } from '../utils/userMessageIdentity';
import { timelineNodeKey } from '../utils/subagentGrouping';
import { itemTimelineStructureChanged } from '../utils/timelineStructure';
import { cursorFromItem, cursorsAfterItemUpserts, mergeItemsById, mergeMissingItemsById, reconcileSnapshotPage } from './threadItems';
import { applyItemUpsertsToWindow } from './threadItemUpserts';

function send(id: string, itemIndex = 0, overrides: Partial<Item> = {}): Item {
  return makeItem({ id, turnIndex: 2, itemIndex, kind: 'user_text', role: 'user',
    meta: JSON.stringify({ sendId: 'send-1' }), ...overrides });
}

function apply(current: Item[], incoming: Item[]) {
  return applyItemUpsertsToWindow({ current, incoming,
    itemIndexById: new Map(current.map((item, index) => [item.id, index])),
    optimisticItemIds: new Set(current.filter(item => item.id.startsWith('optimistic:')).map(item => item.id)),
    currentThreadId: 'thread-1', oldestLoadedCursor: cursorFromItem(current[0]),
    newestLoadedCursor: cursorFromItem(current.at(-1)!), hasMoreHistory: true, hasMoreNewer: true });
}

describe('send identity', () => {
  it('is independent of position, backend id and provider metadata', () => {
    const before = send('optimistic:send-1', 12);
    const after = send('user:canonical', -1, { turnIndex: 3, meta: '{"sendId":"send-1","provider_item_id":"provider"}' });
    expect(userMessageIdentity(before)).toBe(userMessageIdentity(after));
    expect(timelineNodeKey({ kind: 'leaf', item: before })).toBe(timelineNodeKey({ kind: 'leaf', item: after }));
    expect(userMessageIdentity(send('other', 0, { threadId: 'other-thread' }))).not.toBe(userMessageIdentity(before));
    expect(userMessageIdentity(send('other', 0, { meta: '{"sendId":"send-2"}' }))).not.toBe(userMessageIdentity(before));
  });

  it.each([
    { kind: 'assistant_text' }, { parentId: 'agent' }, { meta: '{"sendId":"send-1","wire_only":true}' },
    { meta: undefined }, { meta: 'broken' }, { meta: '{"sendId":5}' }, { meta: '{"sendId":""}' },
  ] satisfies Partial<Item>[])('does not correlate unsupported rows: %j', (overrides) => {
    expect(userMessageIdentity(send('row', 0, overrides))).toBeNull();
  });

  it('refreshes cached identity across metadata and thread transitions', () => {
    const item = send('user:1');
    const original = userMessageIdentity(item);
    item.meta = '{"sendId":"send-1","wire_only":true}';
    expect(userMessageIdentity(item)).toBeNull();
    item.meta = '{"sendId":"send-1"}';
    expect(userMessageIdentity(item)).toBe(original);
    item.threadId = 'other';
    expect(userMessageIdentity(item)).not.toBe(original);
    item.threadId = 'thread-1';
    expect(userMessageIdentity(item)).toBe(original);
  });

  it('invalidates structure when identity metadata is added or removed', () => {
    const before = send('user:1', 0, { meta: undefined });
    const after = send('user:1');
    expect(itemTimelineStructureChanged(before, after)).toBe(true);
    expect(itemTimelineStructureChanged(after, before)).toBe(true);
    expect(itemTimelineStructureChanged(after, { ...after, meta: '{"sendId":"send-1","provider_item_id":"ack"}' })).toBe(false);
  });
});

describe('confirmation ordering and page coverage', () => {
  it.each([-1, 11, 20])('moves a confirmed floor to %s without hiding unloaded history', (position) => {
    const current = [send('optimistic:send-1', 10), makeItem({ id: 'middle', turnIndex: 2, itemIndex: 11 }), makeItem({ id: 'tail', turnIndex: 2, itemIndex: 12 })];
    const confirmed = send('user:canonical', position);
    const next = apply(current, [confirmed])!;
    expect(next.appendedItems).toEqual([]);
    expect(next.items).toContain(confirmed);
    expect(next.items.map(item => item.itemIndex)).toEqual([position, 11, 12].sort((a, b) => a - b));
    const cursors = cursorsAfterItemUpserts(cursorFromItem(current[0]), cursorFromItem(current[2]), current, [confirmed], 'thread-1');
    expect(cursors.oldest?.itemIndex).toBe(position < 10 ? 10 : 11);
    expect(cursors.newest?.itemIndex).toBe(12);
  });

  it.each([false, true])('admits a filled extension regardless of event ordering, insert first=%s', (first) => {
    const current = [makeItem({ id: 'head', turnIndex: 2, itemIndex: 10 }), send('optimistic:send-1', 12)];
    const confirmed = send('user:canonical', 14);
    const inserted = makeItem({ id: 'inserted', turnIndex: 2, itemIndex: 13 });
    const incoming = first ? [inserted, confirmed] : [confirmed, inserted];
    const result = apply(current, incoming)!;
    expect(result.items.map(item => item.id)).toEqual(['head', 'inserted', confirmed.id]);
    expect(result.appendedItems).toEqual([inserted]);
    expect(cursorsAfterItemUpserts(cursorFromItem(current[0]), cursorFromItem(current[1]), current, incoming, 'thread-1').newest).toEqual(cursorFromItem(confirmed));
  });

  it('proves a translated span using renamed interior sends as well as endpoints', () => {
    const current = [10, 11, 12].map(i => send(`optimistic:${i}`, i, { meta: JSON.stringify({ sendId: `send-${i}` }) }));
    const incoming = current.map(item => ({ ...item, id: `user:${item.itemIndex}`, itemIndex: item.itemIndex + 5 }));
    expect(cursorsAfterItemUpserts(cursorFromItem(current[0]), cursorFromItem(current[2]), current, incoming, 'thread-1'))
      .toEqual({ oldest: cursorFromItem(incoming[0]), newest: cursorFromItem(incoming[2]) });
    expect(apply(current, incoming)?.items).toEqual(incoming);
  });

  it('deduplicates repeated confirmations within one batch and uses the last position', () => {
    const before = send('optimistic:send-1', 12);
    const confirmed = send('user:canonical', 11);
    const corrected = { ...confirmed, itemIndex: 10 };
    const result = apply([before], [confirmed, confirmed, corrected])!;
    expect(result.items).toEqual([corrected]);
    expect(result.replacedItems).toEqual([before]);
    expect(result.appendedItems).toEqual([]);
  });

  it('rejects conflicting authoritative ids before committing a partial batch', () => {
    const before = send('optimistic:send-1');
    expect(() => apply([before], [send('user:one'), send('user:two')])).toThrow('Conflicting confirmation ids');
    expect(before.id).toBe('optimistic:send-1');
  });

  it('does not match another thread or a parented message to the local send', () => {
    const before = send('optimistic:send-1');
    expect(apply([before], [send('user:other-thread', 0, { threadId: 'other' })])).toBeNull();
    const parented = send('user:agent', 0, { parentId: 'absent' });
    const result = apply([before], [parented]);
    expect(result?.items).toEqual([before]);
    expect(result?.rejectedParentedItems).toEqual([parented]);
  });
});

describe('confirmation through history loading', () => {
  it('a snapshot supersedes the same send even if its provisional row was touched during the fetch', () => {
    const before = send('optimistic:send-1', 12);
    const confirmed = send('user:canonical', 10);
    expect(reconcileSnapshotPage([confirmed], [before], new Set([before.id])).items).toEqual([confirmed]);
    expect(mergeItemsById([confirmed], [before])).toEqual([confirmed]);
    expect(mergeItemsById([confirmed, confirmed], [before])).toEqual([confirmed]);
  });

  it('missing-row merges retain the current representation of an already displayed send', () => {
    const before = send('optimistic:send-1', 12);
    const deferred = send('user:canonical', 10);
    const current = [before];
    expect(mergeMissingItemsById([deferred], current)).toBe(current);
    expect(mergeMissingItemsById([before], [deferred])).toEqual([deferred]);
  });
  it('does not retain a stale send alias when a same-id update changes its metadata', () => {
    const before = send('user:one');
    const changed = { ...before, meta: '{"sendId":"send-2"}' };
    const originalSend = send('user:two', 1);
    expect(mergeItemsById([changed, originalSend], [before])).toEqual([changed, originalSend]);
  });

});
