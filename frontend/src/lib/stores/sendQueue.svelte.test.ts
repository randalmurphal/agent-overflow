import { describe, it, expect, beforeEach } from 'vitest';
import {
  clearForThread,
  combineForRetract,
  confirmFlushedByUserItemId,
  getFlushedForThread,
  getQueueRevisionForThread,
  getQueueForThread,
  hasQueueItems,
  hasRetractableQueueItems,
  markItemsFlushed,
  replaceFlushedForThread,
  replaceQueueForThread,
  resetForTest as resetSendQueueForTest,
  type QueueItem,
} from './sendQueue.svelte';
import type { Attachment } from '../types/attachment';

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

    it('hasRetractableQueueItems is true only when Zone 1 non-empty', () => {
      expect(hasRetractableQueueItems('t1')).toBe(false);
      replaceQueueForThread('t1', [makeItem({ threadId: 't1', message: 'a' })]);
      expect(hasRetractableQueueItems('t1')).toBe(true);
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

    it('confirming the last entry deletes the map key', () => {
      markItemsFlushed('t1', [{ queueItemId: 'q:0', userItemId: 'u:0', message: 'a' }]);
      confirmFlushedByUserItemId('t1', 'u:0');
      expect(getFlushedForThread('t1')).toHaveLength(0);
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

  describe('combineForRetract', () => {
    it('joins messages with double-newlines', () => {
      const items: QueueItem[] = [
        makeItem({ threadId: 't1', message: 'first' }),
        makeItem({ threadId: 't1', message: 'second' }),
      ];
      const snap = combineForRetract(items, () => undefined);
      expect(snap.content).toBe('first\n\nsecond');
    });

    it('deduplicates attachments by id', () => {
      const att: Attachment = {
        id: 'a1',
        threadId: 't1',
        filename: 'f.png',
        mimeType: 'image/png',
        size: 1,
      } as Attachment;
      const items: QueueItem[] = [
        makeItem({ threadId: 't1', message: 'a', attachmentIds: ['a1'] }),
        makeItem({ threadId: 't1', message: 'b', attachmentIds: ['a1'] }),
      ];
      const snap = combineForRetract(items, (id) => (id === 'a1' ? att : undefined));
      expect(snap.attachments).toHaveLength(1);
      expect(snap.attachments[0].id).toBe('a1');
    });

    it('skips attachments the resolver returns undefined for', () => {
      const items: QueueItem[] = [makeItem({ threadId: 't1', message: 'a', attachmentIds: ['a1'] })];
      const snap = combineForRetract(items, () => undefined);
      expect(snap.attachments).toHaveLength(0);
    });
  });
});
