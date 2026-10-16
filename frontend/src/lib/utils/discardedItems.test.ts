import { describe, expect, it } from 'vitest';
import { makeItem } from '../../test/helpers/chat';
import type { Item } from '../types/models';
import {
  abandonedItemIds,
  abandonedWholeResponse,
  assistantRowAfter,
  discardedItemIds,
} from './discardedItems';

function record(itemIds: string[], fromBlockIndex: number, overrides: Partial<Item> = {}): Item {
  return makeItem({
    id: `notification:0:abandoned:msg_01`,
    kind: 'notification',
    role: 'system',
    summary: 'Discarded by Claude after a stalled response',
    meta: JSON.stringify({
      kind: 'abandoned_blocks',
      title: 'Discarded by Claude after a stalled response',
      api_message_id: 'msg_01',
      from_block_index: fromBlockIndex,
      item_ids: itemIds,
    }),
    ...overrides,
  });
}

describe('discardedItems', () => {
  it('reads the discarded row ids only off an abandoned_blocks notification', () => {
    expect(abandonedItemIds(record(['text:0:1', 'think:0:1'], 0))).toEqual(['text:0:1', 'think:0:1']);
    expect(abandonedItemIds(makeItem({ kind: 'notification', meta: '{"kind":"session_died"}' }))).toEqual([]);
    expect(abandonedItemIds(makeItem({ kind: 'assistant_text', meta: '{"kind":"abandoned_blocks","item_ids":["x"]}' }))).toEqual([]);
    expect(abandonedItemIds(record([], 0, { meta: '{"kind":"abandoned_blocks","item_ids":"text:0:1"}' }))).toEqual([]);
  });

  it('tells a whole-response discard from a tail', () => {
    expect(abandonedWholeResponse(record(['text:0:1'], 0))).toBe(true);
    expect(abandonedWholeResponse(record(['text:0:2'], 1))).toBe(false);
  });

  it('unions every loaded record and memoizes on the array identity', () => {
    const a = makeItem({ id: 'text:0:1' });
    const b = makeItem({ id: 'text:0:2' });
    const items = [a, record(['text:0:1'], 0), b, record(['text:0:2'], 1, { id: 'notification:0:abandoned:msg_02' })];
    const ids = discardedItemIds(items);
    expect([...ids].sort()).toEqual(['text:0:1', 'text:0:2']);
    expect(discardedItemIds(items)).toBe(ids);
    // A new array (membership change) is scanned afresh.
    const fewer = items.slice(0, 1);
    expect(discardedItemIds(fewer).size).toBe(0);
    expect(discardedItemIds([]).size).toBe(0);
  });

  it('finds what Claude wrote next in the record scope and stops at the next user message', () => {
    const rec = record(['text:0:1'], 0);
    const retry = makeItem({ id: 'text:0:2', summary: 'Retried from scratch' });
    expect(assistantRowAfter([makeItem({ id: 'text:0:1' }), rec, retry], rec)).toBe(retry);
    // A scoped row after the record belongs to another scope.
    const scoped = makeItem({ id: 'text:0:tool:1', parentId: 'toolu_1' });
    expect(assistantRowAfter([rec, scoped], rec)).toBeNull();
    // Nothing followed, or the user spoke first.
    expect(assistantRowAfter([rec], rec)).toBeNull();
    expect(assistantRowAfter([rec, makeItem({ id: 'user:1', kind: 'user_text', role: 'user' }), retry], rec)).toBeNull();
    // A tool call is a continuation too.
    const tool = makeItem({ id: 'toolu_2', kind: 'tool_call' });
    expect(assistantRowAfter([rec, tool], rec)).toBe(tool);
    // A record outside the window answers nothing.
    expect(assistantRowAfter([retry], rec)).toBeNull();
  });
});
