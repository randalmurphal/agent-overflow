import { beforeEach, describe, expect, it } from 'vitest';
import { buildPane, makeItem, makeThread } from '../../test/helpers/chat';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { applyQueueFlushed, applyQueueStateChanged } from './eventsQueue';
import { getQueueForThread } from './sendQueue.svelte';
import type { ThreadPane } from './thread.svelte';

function userRow(pane: ThreadPane, sendId: string, overrides: Parameters<typeof makeItem>[0] = {}) {
  return makeItem({ id: `user:${sendId}`, threadId: pane.threadId!, kind: 'user_text', role: 'user',
    status: 'completed', summary: 'Hello', meta: JSON.stringify({ sendId }), ...overrides });
}
function queued(threadId: string, sendId: string) {
  return { id: `queue:${sendId}`, threadId, sendId, message: 'Hello', attachmentIds: [], enqueuedAt: 1 };
}
function flushed(threadId: string, sendId: string) {
  return { threadId, items: [{ queueItemId: `queue:${sendId}`, userItemId: `user:${sendId}`, sendId, message: 'Hello' }] };
}
async function awaitingPane(items: ReturnType<typeof makeItem>[] = []): Promise<ThreadPane> {
  const pane = await buildPane(makeThread(), items);
  expect(pane.sendAwaitingConfirmation).toBe(false);
  pane.awaitSendConfirmation('mine');
  expect(pane.sendAwaitingConfirmation).toBe(true);
  return pane;
}

describe('a dispatched send awaiting its confirmation', () => {
  beforeEach(installThreadPaneTestEnv);

  it('is confirmed by its pushed row, and holds no row of its own until then', async () => {
    const pane = await awaitingPane();
    expect(pane.items).toEqual([]);
    const row = userRow(pane, 'mine');
    pane.applyProviderItemUpserts([row]);
    expect(pane.sendAwaitingConfirmation).toBe(false);
    expect(pane.items).toEqual([row]);
  });

  it('is confirmed by its pushed row when the window does not admit the row', async () => {
    const pane = await awaitingPane([makeItem({ id: 'loaded', turnIndex: 5 })]);
    // Below the loaded window's floor: the row is real but not held here.
    expect(pane.applyProviderItemUpserts([userRow(pane, 'mine', { turnIndex: 0 })])).toBeNull();
    expect(pane.items.map((item) => item.id)).toEqual(['loaded']);
    expect(pane.sendAwaitingConfirmation).toBe(false);
  });

  it('is not confirmed by another send’s row, an assistant row or a history read', async () => {
    const pane = await awaitingPane();
    pane.applyProviderItemUpserts([userRow(pane, 'theirs')]);
    pane.applyProviderItemUpserts([makeItem({ id: 'assistant', threadId: pane.threadId!, turnIndex: 1,
      meta: JSON.stringify({ sendId: 'mine' }) })]);
    // Only the wire confirms: a row merged from a read is not evidence
    // the backend took this dispatch.
    pane.upsertItems([userRow(pane, 'mine')]);
    expect(pane.sendAwaitingConfirmation).toBe(true);
  });

  it('is confirmed by a queue state naming it', async () => {
    const pane = await awaitingPane();
    applyQueueStateChanged({ threadId: pane.threadId!, items: [queued(pane.threadId!, 'theirs')] });
    expect(pane.sendAwaitingConfirmation).toBe(true);
    applyQueueStateChanged({ threadId: pane.threadId!, items: [queued(pane.threadId!, 'theirs'), queued(pane.threadId!, 'mine')] });
    expect(pane.sendAwaitingConfirmation).toBe(false);
    expect(getQueueForThread(pane.threadId).map((item) => item.sendId)).toEqual(['theirs', 'mine']);
    expect(pane.items).toEqual([]);
  });

  it('is confirmed by a flush naming it', async () => {
    const pane = await awaitingPane();
    applyQueueFlushed(flushed(pane.threadId!, 'theirs'));
    expect(pane.sendAwaitingConfirmation).toBe(true);
    applyQueueFlushed(flushed(pane.threadId!, 'mine'));
    expect(pane.sendAwaitingConfirmation).toBe(false);
    expect(pane.items).toEqual([]);
  });

  it('is not confirmed by queue events of another thread carrying the same send id', async () => {
    const pane = await awaitingPane();
    applyQueueStateChanged({ threadId: 'another-thread', items: [queued('another-thread', 'mine')] });
    applyQueueFlushed(flushed('another-thread', 'mine'));
    expect(pane.sendAwaitingConfirmation).toBe(true);
  });

  it.each(['queueItems', 'flushedItems'] as const)('is confirmed by a live-state read listing it in %s', async (field) => {
    const pane = await awaitingPane();
    const threadId = pane.threadId!;
    const live = (sendId: string) => ({ threadId, activeTurn: null,
      queueItems: field === 'queueItems' ? [queued(threadId, sendId)] : [],
      flushedItems: field === 'flushedItems' ? flushed(threadId, sendId).items : [] });
    setBindingMock('GetThreadLiveState', async () => live('theirs'));
    await pane.refreshFromBackend();
    expect(pane.sendAwaitingConfirmation).toBe(true);
    setBindingMock('GetThreadLiveState', async () => live('mine'));
    await pane.refreshFromBackend();
    expect(pane.sendAwaitingConfirmation).toBe(false);
  });

  it('is confirmed directly only for its own send on its own thread', async () => {
    const pane = await awaitingPane();
    pane.confirmSend('another-thread', 'mine');
    pane.confirmSend(pane.threadId!, 'theirs');
    pane.confirmSend(pane.threadId!, undefined);
    expect(pane.sendAwaitingConfirmation).toBe(true);
    pane.confirmSend(pane.threadId!, 'mine');
    expect(pane.sendAwaitingConfirmation).toBe(false);
  });

  it('follows the latest dispatch', async () => {
    const pane = await awaitingPane();
    pane.awaitSendConfirmation('next');
    pane.confirmSend(pane.threadId!, 'mine');
    expect(pane.sendAwaitingConfirmation).toBe(true);
    pane.confirmSend(pane.threadId!, 'next');
    expect(pane.sendAwaitingConfirmation).toBe(false);
  });

  it('does not carry into another thread', async () => {
    const pane = await awaitingPane();
    const other = makeThread({ id: 'thread-2' });
    setBindingMock('SwitchThread', async () => other);
    await pane.switchThread(other);
    expect(pane.threadId).toBe('thread-2');
    expect(pane.sendAwaitingConfirmation).toBe(false);
  });

  it('ends when the pane is cleared', async () => {
    const pane = await awaitingPane();
    pane.clear();
    expect(pane.sendAwaitingConfirmation).toBe(false);
  });
});
