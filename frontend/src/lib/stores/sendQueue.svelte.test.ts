import { beforeEach, describe, expect, it } from 'vitest';
import {
  cancelItem,
  clearForThread,
  enqueue,
  enqueueAtFront,
  getQueueForThread,
  hasQueueItems,
  popFront,
  popItem,
  resetSendQueueForTest,
  snapshotFromQueueItem,
  type QueueItem,
} from './sendQueue.svelte';
import type { Attachment } from '../types/attachment';

function makeAttachment(id: string): Attachment {
  return {
    id,
    threadId: 'thread-1',
    filename: `${id}.png`,
    mimeType: 'image/png',
    size: 128,
    relativePath: `thread-1/${id}.png`,
    createdAt: 1,
  };
}

function baseDraft(message = 'hello'): Omit<QueueItem, 'id' | 'enqueuedAt'> {
  return {
    message,
    attachments: [],
    terminalChips: [],
    sourceProposedPlan: null,
  };
}

describe('sendQueue store', () => {
  beforeEach(() => {
    resetSendQueueForTest();
  });

  it('returns an empty array for unknown threads', () => {
    expect(getQueueForThread('thread-x')).toEqual([]);
    expect(hasQueueItems('thread-x')).toBe(false);
  });

  it('treats null/undefined threadIds as empty', () => {
    expect(hasQueueItems(null)).toBe(false);
    expect(hasQueueItems(undefined)).toBe(false);
    expect(getQueueForThread('')).toEqual([]);
  });

  it('enqueue appends in FIFO order with stable ids and timestamps', () => {
    const idA = enqueue('thread-1', baseDraft('first'));
    const idB = enqueue('thread-1', baseDraft('second'));
    const idC = enqueue('thread-1', baseDraft('third'));

    const items = getQueueForThread('thread-1');
    expect(items.map((item) => item.id)).toEqual([idA, idB, idC]);
    expect(items.map((item) => item.message)).toEqual(['first', 'second', 'third']);
    expect(items.every((item) => item.enqueuedAt > 0)).toBe(true);
    expect(idA).not.toBe(idB);
    expect(idB).not.toBe(idC);
  });

  it('enqueue refuses an empty threadId', () => {
    expect(() => enqueue('', baseDraft())).toThrow(/threadId/);
  });

  it('enqueue captures runtimeMode so the drain path can replay a staged AccessToggle change', () => {
    enqueue('thread-1', {
      message: 'staged mode',
      attachments: [],
      terminalChips: [],
      sourceProposedPlan: null,
      runtimeMode: 'auto-accept-edits',
    });
    expect(getQueueForThread('thread-1')[0].runtimeMode).toBe('auto-accept-edits');
  });

  it('enqueue stores undefined runtimeMode when no override is staged', () => {
    enqueue('thread-1', baseDraft());
    expect(getQueueForThread('thread-1')[0].runtimeMode).toBeUndefined();
  });

  it('enqueue clones attachments / terminalChips / comment ids so caller mutations are safe', () => {
    const attachments = [makeAttachment('a-1')];
    const chips = [{ id: 'chip-1', label: 'term', preview: 'p', content: 'c', createdAt: 1 }];
    const commentIds = ['c-1'];

    enqueue('thread-1', {
      message: 'hello',
      attachments,
      terminalChips: chips,
      sourceProposedPlan: null,
      revisionSourceCommentIds: commentIds,
    });

    // Mutate caller-provided arrays AFTER enqueue. The store must hold
    // its own copy so the queue isn't subject to outside churn.
    attachments.push(makeAttachment('a-2'));
    chips.push({ id: 'chip-2', label: '2', preview: '2', content: '2', createdAt: 2 });
    commentIds.push('c-2');

    const stored = getQueueForThread('thread-1')[0];
    expect(stored.attachments).toHaveLength(1);
    expect(stored.terminalChips).toHaveLength(1);
    expect(stored.revisionSourceCommentIds).toEqual(['c-1']);
  });

  it('popFront removes the head and returns it', () => {
    enqueue('thread-1', baseDraft('first'));
    enqueue('thread-1', baseDraft('second'));

    const head = popFront('thread-1');
    expect(head?.message).toBe('first');
    expect(getQueueForThread('thread-1').map((item) => item.message)).toEqual(['second']);
  });

  it('popFront on an empty queue returns undefined and leaves state untouched', () => {
    expect(popFront('thread-1')).toBeUndefined();
    expect(getQueueForThread('thread-1')).toEqual([]);
  });

  it('popFront drops the queue entry entirely when the last item is taken', () => {
    enqueue('thread-1', baseDraft('only'));
    expect(hasQueueItems('thread-1')).toBe(true);
    popFront('thread-1');
    expect(hasQueueItems('thread-1')).toBe(false);
  });

  it('popItem lifts an arbitrary item out by id', () => {
    const idA = enqueue('thread-1', baseDraft('first'));
    const idB = enqueue('thread-1', baseDraft('second'));
    const idC = enqueue('thread-1', baseDraft('third'));

    const middle = popItem('thread-1', idB);
    expect(middle?.message).toBe('second');
    expect(getQueueForThread('thread-1').map((item) => item.id)).toEqual([idA, idC]);
  });

  it('popItem returns undefined when the id is not present', () => {
    enqueue('thread-1', baseDraft('first'));
    expect(popItem('thread-1', 'missing')).toBeUndefined();
    expect(getQueueForThread('thread-1')).toHaveLength(1);
  });

  it('cancelItem removes only that id and reports whether it found one', () => {
    const idA = enqueue('thread-1', baseDraft('first'));
    const idB = enqueue('thread-1', baseDraft('second'));

    expect(cancelItem('thread-1', idA)).toBe(true);
    expect(cancelItem('thread-1', idA)).toBe(false);
    expect(getQueueForThread('thread-1').map((item) => item.id)).toEqual([idB]);
  });

  it('enqueueAtFront preserves existing item identity for drain failure recovery', () => {
    const idA = enqueue('thread-1', baseDraft('first'));
    enqueue('thread-1', baseDraft('second'));

    const restored = popFront('thread-1');
    expect(restored).toBeDefined();
    enqueueAtFront('thread-1', restored as QueueItem);

    const items = getQueueForThread('thread-1');
    expect(items.map((item) => item.message)).toEqual(['first', 'second']);
    // Same id (no fresh mint) so the rendered row doesn't visibly
    // reshuffle when the user restores after a drain failure.
    expect(items[0].id).toBe(idA);
  });

  it('clearForThread sweeps all items for that thread only', () => {
    enqueue('thread-1', baseDraft('a'));
    enqueue('thread-1', baseDraft('b'));
    enqueue('thread-2', baseDraft('c'));

    clearForThread('thread-1');
    expect(getQueueForThread('thread-1')).toEqual([]);
    expect(getQueueForThread('thread-2').map((item) => item.message)).toEqual(['c']);
  });

  it('snapshotFromQueueItem builds the composer-restore shape', () => {
    const id = enqueue('thread-1', {
      message: 'hello',
      attachments: [makeAttachment('a-1')],
      terminalChips: [{ id: 'chip-1', label: 'term', preview: 'p', content: 'c', createdAt: 1 }],
      sourceProposedPlan: { threadId: 'thread-1', itemId: 'plan-1', payloadId: 'p-1', title: 'Plan' },
      // Plan-revision routing metadata is send-only.
      revisionSourceProposedPlan: { threadId: 'thread-1', itemId: 'plan-1', payloadId: 'p-1', title: 'Plan' },
      revisionSourceCommentIds: ['c-1'],
    });
    const item = popItem('thread-1', id);
    if (!item) throw new Error('unexpected pop');

    const snapshot = snapshotFromQueueItem(item);
    expect(snapshot.content).toBe('hello');
    expect(snapshot.attachments.map((a) => a.id)).toEqual(['a-1']);
    expect(snapshot.terminalChips.map((c) => c.id)).toEqual(['chip-1']);
    expect(snapshot.sourceProposedPlan).toMatchObject({ itemId: 'plan-1' });
    // Revision metadata is intentionally absent — it's send-only.
    expect((snapshot as unknown as Record<string, unknown>).revisionSourceProposedPlan).toBeUndefined();
    expect((snapshot as unknown as Record<string, unknown>).revisionSourceCommentIds).toBeUndefined();
  });

  it('queues for different threads are independent', () => {
    enqueue('thread-1', baseDraft('a'));
    enqueue('thread-2', baseDraft('b'));
    expect(getQueueForThread('thread-1').map((i) => i.message)).toEqual(['a']);
    expect(getQueueForThread('thread-2').map((i) => i.message)).toEqual(['b']);

    popFront('thread-1');
    expect(hasQueueItems('thread-1')).toBe(false);
    expect(hasQueueItems('thread-2')).toBe(true);
  });
});
