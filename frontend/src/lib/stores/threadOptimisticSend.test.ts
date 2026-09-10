import { beforeEach, describe, expect, it, vi } from 'vitest';
import { buildPane, makeItem, makeThread } from '../../test/helpers/chat';
import { flushMicrotasks, installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { applyQueueFlushed, applyQueueStateChanged } from './eventsQueue';
import { getQueueForThread } from './sendQueue.svelte';
import { dispatchSend } from '../components/composer/composerSend';
import { buildSendOptions } from '../utils/sendOptions';
import type { ThreadPane } from './thread.svelte';

function optimistic(pane: ThreadPane, sendId: string, id = `optimistic:${sendId}`) {
  const item = makeItem({ id, threadId: pane.threadId!, kind: 'user_text', role: 'user',
    status: 'completed', turnIndex: (pane.items.at(-1)?.turnIndex ?? -1) + 1, summary: 'Hello', meta: JSON.stringify({ sendId }) });
  pane.trackOptimisticItem(id);
  pane.upsertItems([item]);
  return item;
}
function canonical(pane: ThreadPane, sendId: string, id = 'user:0:flush:1') {
  return makeItem({ id, threadId: pane.threadId!, kind: 'user_text', role: 'user',
    status: 'completed', summary: 'Hello', meta: JSON.stringify({ sendId, provider_item_id: 'provider-1' }) });
}
function queued(threadId: string, sendId: string) {
  return { id: 'queue:1', threadId, sendId, message: 'Hello', attachmentIds: [], enqueuedAt: 1 };
}

describe('optimistic send acknowledgement', () => {
  beforeEach(installThreadPaneTestEnv);

  it.each(['before', 'after'] as const)('keeps the placeholder until a canonical queue event %s the RPC response', async (eventOrder) => {
    const pane = await buildPane();
    const options = buildSendOptions({ attachmentIds: [] });
    const placeholder = optimistic(pane, options.sendId);
    let finish!: () => void;
    const send = setBindingMock('SendMessageWithOptions', async () => {
      await new Promise<void>((resolve) => { finish = resolve; });
      return makeThread();
    });
    const pending = dispatchSend({ threadId: pane.threadId!, message: 'Hello', options,
      snapshot: { content: 'Hello', attachments: [], terminalChips: [] }, restoreDraft: vi.fn(),
      draftThreadId: () => pane.threadId, reportError: vi.fn() });
    expect(send).toHaveBeenCalledWith(pane.threadId, 'Hello', options);
    if (eventOrder === 'after') {
      finish();
      expect(await pending).toBe(true);
      expect(pane.getItemById(placeholder.id)).toBeDefined();
    }
    applyQueueStateChanged({ threadId: pane.threadId!, items: [queued(pane.threadId!, options.sendId)] });
    expect(pane.getItemById(placeholder.id)).toBeUndefined();
    expect(pane.isOptimisticItem(placeholder.id)).toBe(false);
    expect(getQueueForThread(pane.threadId)).toHaveLength(1);
    if (eventOrder === 'before') {
      finish();
      expect(await pending).toBe(true);
    }
    expect(pane.items).toHaveLength(0);
  });

  it('reconciles a flushed event without requiring the queued event and tolerates replay', async () => {
    const pane = await buildPane();
    const placeholder = optimistic(pane, 'mine');
    const event = { threadId: pane.threadId!, items: [{ queueItemId: 'queue:1', userItemId: 'user:0:flush:1', sendId: 'mine', message: 'Hello' }] };
    applyQueueFlushed(event);
    applyQueueFlushed(event);
    expect(pane.getItemById(placeholder.id)).toBeUndefined();
    pane.applyProviderItemUpserts([canonical(pane, 'mine')]);
    applyQueueFlushed(event);
    expect(pane.items.map((item) => item.id)).toEqual(['user:0:flush:1']);
  });

  it('replaces a predicted later turn with the same send at the actual active-turn position', async () => {
    const pane = await buildPane(makeThread(), [makeItem({ id: 'active', itemIndex: 1 })]);
    optimistic(pane, 'mine');
    pane.applyProviderItemUpserts([canonical(pane, 'mine')]);
    pane.applyProviderItemUpserts([canonical(pane, 'mine')]);
    expect(pane.items.map((item) => item.id)).toEqual(['user:0:flush:1', 'active']);
    expect(pane.debugMemoryStats().optimisticItems).toBe(0);
  });

  it('confirms a displayed send in one commit without appending or disposing its presentation', async () => {
    const pane = await buildPane();
    const placeholder = optimistic(pane, 'mine');
    pane.setUserMessageExpanded(placeholder.id, true);
    const cache = pane.attachmentCacheFor(placeholder.id);
    const preview = { id: 'attachment', filename: 'image.png', mimeType: 'image/png', size: 1, url: 'data:image/png;base64,AA==' };
    cache.set('attachment', preview);
    const revision = pane.timelineRevision;
    const confirmed = canonical(pane, 'mine');

    const result = pane.applyProviderItemUpserts([confirmed]);

    expect(pane.timelineRevision).toBe(revision + 1);
    expect(result?.appendedItems).toEqual([]);
    expect(pane.items).toEqual([confirmed]);
    expect(pane.getItemById(placeholder.id)).toBeUndefined();
    expect(pane.getItemById(confirmed.id)).toEqual(confirmed);
    expect(pane.isUserMessageExpanded(confirmed.id)).toBe(true);
    expect(pane.attachmentCacheFor(confirmed.id).get('attachment')).toEqual(preview);
    expect(cache.get('attachment')).toEqual(preview);
    expect(pane.applyProviderItemUpserts([confirmed])).toBeNull();
    expect(pane.timelineRevision).toBe(revision + 1);

    pane.removeItemById(confirmed.id, pane.threadId!);
    expect(cache.get('attachment')).toBeUndefined();
    expect(pane.isUserMessageExpanded(confirmed.id)).toBe(false);
    cache.set('late', preview);
    expect(pane.attachmentCacheFor(confirmed.id).get('late')).toBeUndefined();
  });

  it('keeps another client’s row and this client’s pending send separate', async () => {
    const pane = await buildPane();
    const placeholder = optimistic(pane, 'mine');
    pane.applyProviderItemUpserts([canonical(pane, 'theirs', 'user:1')]);
    applyQueueStateChanged({ threadId: pane.threadId!, items: [queued(pane.threadId!, 'theirs')] });
    expect(pane.getItemById(placeholder.id)).toBeDefined();
    pane.applyProviderItemUpserts([canonical(pane, 'mine')]);
    expect(pane.items.map((item) => item.id).sort()).toEqual(['user:0:flush:1', 'user:1']);
  });

  it('never deletes a persisted row or a placeholder from another thread', async () => {
    const pane = await buildPane();
    const placeholder = optimistic(pane, 'mine');
    pane.confirmOptimisticSend('another-thread', 'mine');
    pane.confirmOptimisticSend(pane.threadId!, undefined);
    expect(pane.getItemById(placeholder.id)).toBeDefined();
    pane.applyProviderItemUpserts([canonical(pane, 'mine', placeholder.id)]);
    expect(pane.isOptimisticItem(placeholder.id)).toBe(false);
    pane.confirmOptimisticSend(pane.threadId!, 'mine');
    applyQueueStateChanged({ threadId: pane.threadId!, items: [queued(pane.threadId!, 'mine')] });
    expect(pane.getItemById(placeholder.id)).toBeDefined();
  });

  it('retains same-ID placeholders until the authoritative row replaces them', async () => {
    const pane = await buildPane();
    optimistic(pane, 'mine', 'user:1');
    pane.confirmOptimisticSend(pane.threadId!, 'mine', 'user:1');
    expect(pane.isOptimisticItem('user:1')).toBe(true);
    pane.applyProviderItemUpserts([canonical(pane, 'mine', 'user:1')]);
    expect(pane.items).toHaveLength(1);
    expect(pane.isOptimisticItem('user:1')).toBe(false);
  });

  it('reconciles a current queue snapshot after missing its live event', async () => {
    const pane = await buildPane();
    const placeholder = optimistic(pane, 'mine');
    setBindingMock('GetThreadLiveState', async () => ({ threadId: pane.threadId,
      queueItems: [queued(pane.threadId!, 'mine')], flushedItems: [], deferredItems: [] }));
    await pane.refreshFromBackend();
    expect(pane.getItemById(placeholder.id)).toBeUndefined();
    expect(getQueueForThread(pane.threadId)[0]?.sendId).toBe('mine');
  });
  it.each(['before', 'during'] as const)('confirms a send created %s a history refresh without duplicating or resetting it', async (timing) => {
    const pane = await buildPane();
    let placeholder = timing === 'before' ? optimistic(pane, 'mine') : undefined;
    let release!: (page: { items: ReturnType<typeof makeItem>[]; hasMoreOlder: boolean; hasMoreNewer: boolean }) => void;
    setBindingMock('ListThreadSliceAround', () => new Promise(resolve => { release = resolve; }));
    const refreshing = pane.refreshFromBackend();
    await flushMicrotasks();
    placeholder ??= optimistic(pane, 'mine');
    pane.setUserMessageExpanded(placeholder.id, true);
    const confirmed = canonical(pane, 'mine');
    release({ items: [confirmed], hasMoreOlder: false, hasMoreNewer: false });
    await refreshing;
    expect(pane.items).toEqual([confirmed]);
    expect(pane.isUserMessageExpanded(confirmed.id)).toBe(true);
    expect(pane.debugMemoryStats().optimisticItems).toBe(0);
    expect(pane.applyProviderItemUpserts([confirmed])).toBeNull();
  });

  it('does not let a snapshot in flight undo a later confirmation and position correction', async () => {
    const pane = await buildPane();
    const placeholder = optimistic(pane, 'mine');
    let release!: (page: { items: ReturnType<typeof makeItem>[]; hasMoreOlder: boolean; hasMoreNewer: boolean }) => void;
    setBindingMock('ListThreadSliceAround', () => new Promise(resolve => { release = resolve; }));
    const refreshing = pane.refreshFromBackend();
    await flushMicrotasks();
    const snapshotRow = canonical(pane, 'mine');
    const confirmed = { ...snapshotRow, turnIndex: 3, itemIndex: -1 };
    pane.applyProviderItemUpserts([confirmed]);
    release({ items: [snapshotRow], hasMoreOlder: false, hasMoreNewer: false });
    await refreshing;
    expect(pane.items).toEqual([confirmed]);
    expect(pane.getItemById(placeholder.id)).toBeUndefined();
  });

  it('keeps two identical messages distinct through reversed confirmations', async () => {
    const pane = await buildPane();
    const first = optimistic(pane, 'first');
    const second = optimistic(pane, 'second');
    pane.setUserMessageExpanded(first.id, true);
    const confirmedFirst = { ...canonical(pane, 'first', 'user:first'), itemIndex: 1 };
    const confirmedSecond = { ...canonical(pane, 'second', 'user:second'), itemIndex: 2 };
    pane.applyProviderItemUpserts([confirmedSecond]);
    pane.applyProviderItemUpserts([confirmedFirst]);
    expect(pane.items).toEqual([confirmedFirst, confirmedSecond]);
    expect(pane.isUserMessageExpanded(confirmedFirst.id)).toBe(true);
    expect(pane.isUserMessageExpanded(confirmedSecond.id)).toBe(false);
    expect(pane.getItemById(second.id)).toBeUndefined();
    expect(pane.debugMemoryStats().optimisticItems).toBe(0);
  });

  it('discharges an identical same-id echo even when its item write is a no-op', async () => {
    const pane = await buildPane();
    const placeholder = optimistic(pane, 'mine', 'user:canonical');
    expect(pane.applyProviderItemUpserts([{ ...placeholder }])).toBeNull();
    expect(pane.isOptimisticItem(placeholder.id)).toBe(false);
  });

});
