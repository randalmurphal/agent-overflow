import { describe, it, expect, beforeEach } from 'vitest';
import {
  clearForThread,
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

    it('does not add a flushed marker when confirmation arrives first', () => {
      confirmFlushedByUserItemId('t1', 'u:0');
      markItemsFlushed('t1', [{ queueItemId: 'q:0', userItemId: 'u:0', message: 'a' }]);
      expect(getFlushedForThread('t1')).toHaveLength(0);
    });

    it('filters confirmed ids from replaced flushed snapshots', () => {
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
      expect(getFlushedForThread('t1').map((f) => f.userItemId)).toEqual(['u:1']);
    });

    it('clearForThread clears remembered confirmations', () => {
      confirmFlushedByUserItemId('t1', 'u:0');
      replaceQueueForThread('t1', [makeItem({ threadId: 't1', message: 'queued' })]);
      clearForThread('t1');
      markItemsFlushed('t1', [{ queueItemId: 'q:0', userItemId: 'u:0', message: 'a' }]);
      expect(getFlushedForThread('t1').map((f) => f.userItemId)).toEqual(['u:0']);
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
