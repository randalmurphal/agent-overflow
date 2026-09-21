import { setBindingMock, resetBindingMocks } from '../../test/mocks/bindings-app';
import { buildSendOptions } from '../utils/sendOptions';
import { makeItem as makeChatItem } from '../../test/helpers/chat';
import { describe, it, expect, beforeEach } from 'vitest';
import {
  clearForThread,
  registerQueueItem,
  markQueuedItemConsumed,
  confirmFlushedByUserItemId,
  getFlushedForThread,
  getQueueRevisionForThread,
  getQueueForThread,
  hasQueueItems,
  markItemsFlushed,
  removeRestoredQueueItems,
  replaceFlushedForThread,
  replaceQueueForThread,
  resetForTest as resetSendQueueForTest,
  type QueueItem,
} from './sendQueue.svelte';

function makeItem(overrides: Partial<QueueItem> & { message: string; threadId: string }): QueueItem {
  return {
    id: overrides.id ?? `queue:${overrides.message}`,
    threadId: overrides.threadId,
    message: overrides.message,
    attachmentIds: overrides.attachmentIds ?? [],
    sourceProposedPlan: overrides.sourceProposedPlan ?? null,
    revisionSourceProposedPlan: overrides.revisionSourceProposedPlan ?? null,
    revisionSourceCommentIds: overrides.revisionSourceCommentIds,
    enqueuedAt: overrides.enqueuedAt ?? Date.now(),
  };
}

describe('sendQueue store', () => {
  beforeEach(() => {
    resetSendQueueForTest();
  });

  describe('Zone 1 (queued)', () => {
    it('replaceQueueForThread sets the snapshot', () => {
      const item = makeItem({ threadId: 't1', message: 'hi' });
      replaceQueueForThread('t1', [item]);
      expect(getQueueForThread('t1').map((q) => q.message)).toEqual(['hi']);
    });

    it('replaceQueueForThread with empty list deletes the entry', () => {
      replaceQueueForThread('t1', [makeItem({ threadId: 't1', message: 'a' })]);
      replaceQueueForThread('t1', []);
      expect(getQueueForThread('t1')).toHaveLength(0);
    });

    it('replaceQueueForThread empty no-op does not create revision state', () => {
      replaceQueueForThread('t1', []);
      expect(getQueueRevisionForThread('t1')).toBe(0);
    });

    it('queues are isolated per thread', () => {
      replaceQueueForThread('t1', [makeItem({ threadId: 't1', message: 'a' })]);
      replaceQueueForThread('t2', [makeItem({ threadId: 't2', message: 'b' })]);
      expect(getQueueForThread('t1')).toHaveLength(1);
      expect(getQueueForThread('t2')).toHaveLength(1);
    });
  });

  describe('Zone 2 (flushed)', () => {
    it('markItemsFlushed appends entries', () => {
      markItemsFlushed('t1', [
        { queueItemId: 'queue:0', userItemId: 'user:0:flush:1', message: 'hi' },
      ]);
      const flushed = getFlushedForThread('t1');
      expect(flushed.map((f) => f.userItemId)).toEqual(['user:0:flush:1']);
    });

    it('replaceFlushedForThread replaces the hydrated snapshot', () => {
      markItemsFlushed('t1', [
        { queueItemId: 'queue:old', userItemId: 'user:0:flush:1', message: 'old' },
      ]);
      replaceFlushedForThread('t1', [{
        queueItemId: 'queue:new',
        userItemId: 'user:0:flush:2',
        message: 'new',
        flushedAt: 10,
      }]);
      expect(getFlushedForThread('t1').map((f) => f.message)).toEqual(['new']);
    });

    it('markItemsFlushed with multiple appends in order', () => {
      markItemsFlushed('t1', [
        { queueItemId: 'q:0', userItemId: 'u:0', message: 'a' },
        { queueItemId: 'q:1', userItemId: 'u:1', message: 'b' },
      ]);
      expect(getFlushedForThread('t1').map((f) => f.message)).toEqual(['a', 'b']);
    });

    it('markItemsFlushed moves matching queued rows into flushed state', () => {
      replaceQueueForThread('t1', [
        makeItem({ id: 'q:0', threadId: 't1', message: 'a' }),
        makeItem({ id: 'q:1', threadId: 't1', message: 'b' }),
      ]);
      markItemsFlushed('t1', [{ queueItemId: 'q:0', userItemId: 'u:0', message: 'a' }]);
      expect(getQueueForThread('t1').map((q) => q.id)).toEqual(['q:1']);
      expect(getFlushedForThread('t1').map((f) => f.userItemId)).toEqual(['u:0']);
    });

    it('markItemsFlushed is idempotent for a re-delivered flush event', () => {
      // provider:queue_flushed rides the event ring; a reconnect replay
      // re-delivers the frame. A second append with the same userItemId
      // rendered the message twice and duplicated a keyed-each key.
      const batch = [
        { queueItemId: 'q:0', userItemId: 'u:0', message: 'a' },
        { queueItemId: 'q:1', userItemId: 'u:1', message: 'b' },
      ];
      markItemsFlushed('t1', batch);
      const revisionAfterFirst = getQueueRevisionForThread('t1');
      markItemsFlushed('t1', batch);
      expect(getFlushedForThread('t1').map((f) => f.userItemId)).toEqual(['u:0', 'u:1']);
      expect(getQueueRevisionForThread('t1')).toBe(revisionAfterFirst);
    });

    it('markItemsFlushed drops in-batch duplicates', () => {
      markItemsFlushed('t1', [
        { queueItemId: 'q:0', userItemId: 'u:0', message: 'a' },
        { queueItemId: 'q:0', userItemId: 'u:0', message: 'a' },
      ]);
      expect(getFlushedForThread('t1').map((f) => f.userItemId)).toEqual(['u:0']);
    });

    it('confirmFlushedByUserItemId removes a single entry', () => {
      markItemsFlushed('t1', [
        { queueItemId: 'q:0', userItemId: 'u:0', message: 'a' },
        { queueItemId: 'q:1', userItemId: 'u:1', message: 'b' },
      ]);
      confirmFlushedByUserItemId('t1', 'u:0');
      expect(getFlushedForThread('t1').map((f) => f.userItemId)).toEqual(['u:1']);
    });

    it('confirmFlushedByUserItemId is a no-op for unknown ids', () => {
      markItemsFlushed('t1', [{ queueItemId: 'q:0', userItemId: 'u:0', message: 'a' }]);
      confirmFlushedByUserItemId('t1', 'u:does-not-exist');
      expect(getFlushedForThread('t1')).toHaveLength(1);
    });

    // No confirmation memo: a confirmation with nothing to remove leaves
    // NO state behind, so a later flush of the same send id (the durable
    // `user:flush:<uuid5(sendId)>` is stable across a requeue/retry) still
    // lands in Zone 2. The memo this replaced dropped the retry from Zone 1
    // without ever showing it in Zone 2 — the "neither place" bug. Whether
    // the row is already on screen is the mounted pane's answer, taken
    // straight after this call (thread.svelte.ts syncRenderedFlushRows).
    it('re-flushing a confirmed send id puts it back in Zone 2', () => {
      markItemsFlushed('t1', [{ queueItemId: 'q:0', userItemId: 'u:0', message: 'a' }]);
      confirmFlushedByUserItemId('t1', 'u:0');
      expect(getFlushedForThread('t1')).toHaveLength(0);

      markItemsFlushed('t1', [{ queueItemId: 'q:0', userItemId: 'u:0', message: 'a' }]);
      expect(getFlushedForThread('t1').map((f) => f.userItemId)).toEqual(['u:0']);
    });

    it('adds a flushed marker even when a confirmation arrived first', () => {
      confirmFlushedByUserItemId('t1', 'u:0');
      markItemsFlushed('t1', [{ queueItemId: 'q:0', userItemId: 'u:0', message: 'a' }]);
      expect(getFlushedForThread('t1').map((f) => f.userItemId)).toEqual(['u:0']);
    });

    it('installs replaced flushed snapshots verbatim', () => {
      confirmFlushedByUserItemId('t1', 'u:0');
      replaceFlushedForThread('t1', [
        {
          queueItemId: 'q:0',
          userItemId: 'u:0',
          message: 'a',
          flushedAt: 1,
        },
        {
          queueItemId: 'q:1',
          userItemId: 'u:1',
          message: 'b',
          flushedAt: 1,
        },
      ]);
      expect(getFlushedForThread('t1').map((f) => f.userItemId)).toEqual(['u:0', 'u:1']);
    });

    it('clearForThread leaves no state that suppresses the next flush', () => {
      confirmFlushedByUserItemId('t1', 'u:0');
      replaceQueueForThread('t1', [makeItem({ threadId: 't1', message: 'queued' })]);
      clearForThread('t1');
      markItemsFlushed('t1', [{ queueItemId: 'q:0', userItemId: 'u:0', message: 'a' }]);
      expect(getFlushedForThread('t1').map((f) => f.userItemId)).toEqual(['u:0']);
    });

    // The eager Claude dispatch emits queue_flushed before the provider
    // write settles. When that write fails the backend requeues the item
    // under its original queue id, so the next authoritative Zone 1
    // snapshot names it again — and the message would render twice above
    // the composer, once queued and once flushed. The snapshot wins.
    it('a requeued queue id takes its message back out of Zone 2', () => {
      markItemsFlushed('t1', [
        { queueItemId: 'q:0', userItemId: 'u:0', message: 'requeued' },
        { queueItemId: 'q:1', userItemId: 'u:1', message: 'still flushed' },
      ]);
      replaceQueueForThread('t1', [
        makeItem({ id: 'q:0', threadId: 't1', message: 'requeued' }),
      ]);
      expect(getQueueForThread('t1').map((q) => q.id)).toEqual(['q:0']);
      expect(getFlushedForThread('t1').map((f) => f.userItemId)).toEqual(['u:1']);
    });

    it('an ordinary queue snapshot leaves unrelated Zone 2 entries alone', () => {
      markItemsFlushed('t1', [{ queueItemId: 'q:0', userItemId: 'u:0', message: 'flushed' }]);
      const before = getQueueRevisionForThread('t1');
      replaceQueueForThread('t1', [
        makeItem({ id: 'q:9', threadId: 't1', message: 'newly queued' }),
      ]);
      expect(getFlushedForThread('t1').map((f) => f.userItemId)).toEqual(['u:0']);
      expect(getQueueRevisionForThread('t1')).toBe(before + 1);
    });

    it('confirming the last entry deletes the map key', () => {
      markItemsFlushed('t1', [{ queueItemId: 'q:0', userItemId: 'u:0', message: 'a' }]);
      confirmFlushedByUserItemId('t1', 'u:0');
      expect(getFlushedForThread('t1')).toHaveLength(0);
    });

    it('removeRestoredQueueItems removes restored Zone 1 and Zone 2 markers', () => {
      replaceQueueForThread('t1', [
        makeItem({ id: 'q:queued', threadId: 't1', message: 'queued' }),
        makeItem({ id: 'q:kept', threadId: 't1', message: 'kept queued' }),
      ]);
      markItemsFlushed('t1', [
        { queueItemId: 'q:quiet', userItemId: 'u:quiet', message: 'quiet' },
        { queueItemId: 'q:deferred', userItemId: 'u:deferred', message: 'deferred' },
        { queueItemId: 'q:kept-flushed', userItemId: 'u:kept', message: 'kept flushed' },
      ]);

      removeRestoredQueueItems('t1', {
        queueItemIds: ['q:queued', 'q:quiet'],
        userItemIds: ['u:deferred'],
      });

      expect(getQueueForThread('t1').map((item) => item.id)).toEqual(['q:kept']);
      expect(getFlushedForThread('t1').map((item) => item.userItemId)).toEqual(['u:kept']);
    });

    it('removeRestoredQueueItems is a no-op for unknown ids', () => {
      replaceQueueForThread('t1', [makeItem({ id: 'q:0', threadId: 't1', message: 'queued' })]);
      markItemsFlushed('t1', [{ queueItemId: 'q:1', userItemId: 'u:1', message: 'flushed' }]);
      const revisionBefore = getQueueRevisionForThread('t1');

      removeRestoredQueueItems('t1', {
        queueItemIds: ['missing-queue'],
        userItemIds: ['missing-user'],
      });

      expect(getQueueRevisionForThread('t1')).toBe(revisionBefore);
      expect(getQueueForThread('t1').map((item) => item.id)).toEqual(['q:0']);
      expect(getFlushedForThread('t1').map((item) => item.userItemId)).toEqual(['u:1']);
    });
  });

  describe('hasQueueItems combined predicate', () => {
    it('returns true when only Zone 1 has items', () => {
      replaceQueueForThread('t1', [makeItem({ threadId: 't1', message: 'a' })]);
      expect(hasQueueItems('t1')).toBe(true);
    });

    it('returns true when only Zone 2 has items', () => {
      markItemsFlushed('t1', [{ queueItemId: 'q:0', userItemId: 'u:0', message: 'a' }]);
      expect(hasQueueItems('t1')).toBe(true);
    });

    it('returns false when both zones empty', () => {
      expect(hasQueueItems('t1')).toBe(false);
    });
  });

  describe('clearForThread sweeps both zones', () => {
    it('drops Zone 1 + Zone 2 for the cleared thread only', () => {
      replaceQueueForThread('t1', [makeItem({ threadId: 't1', message: 'a' })]);
      markItemsFlushed('t1', [{ queueItemId: 'q:0', userItemId: 'u:0', message: 'a' }]);
      replaceQueueForThread('t2', [makeItem({ threadId: 't2', message: 'b' })]);
      clearForThread('t1');
      expect(getQueueForThread('t1')).toHaveLength(0);
      expect(getFlushedForThread('t1')).toHaveLength(0);
      expect(getQueueForThread('t2')).toHaveLength(1);
    });
  });

  describe('revision tracking', () => {
    it('bumps when flushed snapshots are replaced or deleted', () => {
      replaceFlushedForThread('t1', []);
      expect(getQueueRevisionForThread('t1')).toBe(0);

      replaceFlushedForThread('t1', [{
        queueItemId: 'q:0',
        userItemId: 'u:0',
        message: 'a',
        flushedAt: 1,
      }]);
      expect(getQueueRevisionForThread('t1')).toBe(1);

      replaceFlushedForThread('t1', []);
      expect(getQueueRevisionForThread('t1')).toBe(2);
    });

    it('bumps markItemsFlushed only when entries are appended', () => {
      markItemsFlushed('t1', []);
      expect(getQueueRevisionForThread('t1')).toBe(0);

      markItemsFlushed('t1', [{ queueItemId: 'q:0', userItemId: 'u:0', message: 'a' }]);
      expect(getQueueRevisionForThread('t1')).toBe(1);
    });

    it('bumps confirmFlushedByUserItemId only when an entry is removed', () => {
      markItemsFlushed('t1', [{ queueItemId: 'q:0', userItemId: 'u:0', message: 'a' }]);
      expect(getQueueRevisionForThread('t1')).toBe(1);

      confirmFlushedByUserItemId('t1', 'u:missing');
      expect(getQueueRevisionForThread('t1')).toBe(1);

      confirmFlushedByUserItemId('t1', 'u:0');
      expect(getQueueRevisionForThread('t1')).toBe(2);
    });

    it('bumps clearForThread only when either zone has entries', () => {
      clearForThread('t1');
      expect(getQueueRevisionForThread('t1')).toBe(0);

      replaceQueueForThread('t1', [makeItem({ threadId: 't1', message: 'a' })]);
      markItemsFlushed('t1', [{ queueItemId: 'q:0', userItemId: 'u:0', message: 'a' }]);
      expect(getQueueRevisionForThread('t1')).toBe(2);

      clearForThread('t1');
      expect(getQueueRevisionForThread('t1')).toBe(3);

      clearForThread('t1');
      expect(getQueueRevisionForThread('t1')).toBe(3);
    });
  });

});

describe('pending submission visibility', () => {
  beforeEach(() => { resetSendQueueForTest(); resetBindingMocks(); });

  it('a live snapshot acknowledges a provisional send before its RPC replies', async () => {
    const options = buildSendOptions({ attachmentIds: [] });
    let reply!: (item: unknown) => void;
    setBindingMock('RegisterQueueItem', () => new Promise(resolve => { reply = resolve; }));
    const pending = registerQueueItem('t1', 'pending', options);
    replaceQueueForThread('t1', []);
    replaceFlushedForThread('t1', [{ queueItemId: 'q1', userItemId: 'u1',
      message: 'pending', sendId: options.sendId, flushedAt: 1 }]);
    expect(getQueueForThread('t1')).toHaveLength(0);
    expect(getFlushedForThread('t1')).toHaveLength(1);
    reply({ id: 'q1', threadId: 't1', message: 'pending', sendId: options.sendId, enqueuedAt: 1 });
    await pending;
    expect(getQueueForThread('t1')).toHaveLength(0);
  });

  it('repeated registration with the same send identity shows one provisional entry', async () => {
    const options = buildSendOptions({ attachmentIds: [] });
    let ready!: () => void;
    const held = new Promise<void>(resolve => { ready = resolve; });
    setBindingMock('RegisterQueueItem', async () => ({ id: 'q1', threadId: 't1', message: 'pending', sendId: options.sendId, enqueuedAt: 1 }));
    const sends = [registerQueueItem('t1', 'pending', options, held), registerQueueItem('t1', 'pending', options, held)];
    expect(getQueueForThread('t1')).toHaveLength(1);
    ready();
    await Promise.all(sends);
    expect(getQueueForThread('t1').map(item => item.id)).toEqual(['q1']);
  });

  it('shows the message during draft preparation and admission, then reconciles by sendId', async () => {
    let prepare!: () => void;
    const ready = new Promise<void>(resolve => { prepare = resolve; });
    let reply!: (value: unknown) => void;
    const rpc = setBindingMock('RegisterQueueItem', () => new Promise(resolve => { reply = resolve; }));
    const options = buildSendOptions({ attachmentIds: [] });
    const pending = registerQueueItem('t1', 'visible throughout', options, ready);
    expect(getQueueForThread('t1').map(item => item.message)).toEqual(['visible throughout']);
    expect(rpc).not.toHaveBeenCalled();
    replaceQueueForThread('t1', []);
    expect(getQueueForThread('t1')).toHaveLength(1);
    prepare();
    await Promise.resolve();
    const accepted = { id: 'queue:accepted', threadId: 't1', sendId: options.sendId, message: 'visible throughout', enqueuedAt: 1, attachmentIds: [] };
    replaceQueueForThread('t1', [accepted]);
    expect(getQueueForThread('t1').map(item => item.id)).toEqual([accepted.id]);
    markItemsFlushed('t1', [{ queueItemId: accepted.id, userItemId: 'user:flush:1', sendId: options.sendId, message: accepted.message }]);
    confirmFlushedByUserItemId('t1', 'user:flush:1');
    reply(accepted);
    await pending;
    expect(hasQueueItems('t1')).toBe(false);
  });

  it.each(['preparation', 'admission'])('removes only its provisional message when %s fails', async (phase) => {
    setBindingMock('RegisterQueueItem', async () => { throw new Error('rejected'); });
    const options = buildSendOptions({ attachmentIds: [] });
    const pending = registerQueueItem('t1', 'failed', options, phase === 'preparation' ? Promise.reject(new Error('rejected')) : undefined);
    replaceQueueForThread('t1', [makeItem({ threadId: 't1', message: 'other message' })]);
    await expect(pending).rejects.toThrow('rejected');
    expect(getQueueForThread('t1').map(item => item.message)).toEqual(['other message']);
  });

  it('uses an admission reply when its queue push has not arrived', async () => {
    const options = buildSendOptions({ attachmentIds: [] });
    setBindingMock('RegisterQueueItem', async () => ({ id: 'queue:accepted', threadId: 't1', message: 'accepted', sendId: options.sendId, enqueuedAt: 1 }));
    await registerQueueItem('t1', 'accepted', options);
    expect(getQueueForThread('t1').map(item => [item.id, item.submitting])).toEqual([['queue:accepted', undefined]]);
    markQueuedItemConsumed(makeChatItem({ id: 'user:flush:1', threadId: 't1', kind: 'user_text', summary: 'accepted', meta: JSON.stringify({ sendId: options.sendId }) }));
    expect(getQueueForThread('t1')).toEqual([]);
    expect(getFlushedForThread('t1')).toHaveLength(1);
  });

  it('a joined Claude echo acknowledges every member before delayed admission replies', async () => {
    const replies: Array<(value: unknown) => void> = [];
    setBindingMock('RegisterQueueItem', () => new Promise(resolve => replies.push(resolve)));
    const options = [buildSendOptions({ attachmentIds: [] }), buildSendOptions({ attachmentIds: [] })];
    const sends = options.map((opts, index) => registerQueueItem('t1', `message ${index}`, opts));
    markQueuedItemConsumed(makeChatItem({ id: 'user:flush:joined', threadId: 't1', kind: 'user_text', summary: 'joined message', meta: JSON.stringify({ sendId: options[0].sendId, joinedSendIds: options.map(item => item.sendId) }) }));
    expect(getQueueForThread('t1')).toEqual([]);
    expect(getFlushedForThread('t1')).toHaveLength(1);
    confirmFlushedByUserItemId('t1', 'user:flush:joined');
    replies.forEach((reply, index) => reply({ id: `queue:${index}`, threadId: 't1', sendId: options[index].sendId, message: `message ${index}`, enqueuedAt: 1 }));
    await Promise.all(sends);
    expect(hasQueueItems('t1')).toBe(false);
  });
});
