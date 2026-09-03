import { beforeEach, describe, expect, it } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import {
  claimLocalReadMarker,
  pendingLocalReadMarker,
  resetLocalReadMarkersForTest,
  withLocalReadMarker,
} from './threadReadWrites';
import { markThreadRead, markThreadUnread, replaceAllThreads, getThreadById } from './threads.svelte';
import type { Thread } from '../types/models';

function mkThread(id: string, lastReadAt: number | undefined): Thread {
  return {
    id,
    projectId: 'project-1',
    title: id,
    provider: 'claude',
    workspacePath: '/tmp/ws',
    createdAt: 0,
    updatedAt: 0,
    lastReadAt,
  } as Thread;
}

describe('threadReadWrites', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetLocalReadMarkersForTest();
    replaceAllThreads([]);
  });

  it('reports no claim for a thread nobody is writing', () => {
    expect(pendingLocalReadMarker('thread-1')).toEqual({ held: false, lastReadAt: undefined });
  });

  it('answers the value the claim was opened at, epoch 0 included', () => {
    const release = claimLocalReadMarker('thread-1', 0);
    expect(pendingLocalReadMarker('thread-1')).toEqual({ held: true, lastReadAt: 0 });
    release();
    expect(pendingLocalReadMarker('thread-1').held).toBe(false);
  });

  it('keeps the claim while a second overlapping write is outstanding', () => {
    // A read-mark settling under a mark-unread must not clear the
    // mark-unread's claim: the newest value is the one that wins, and
    // the last release is the one that ends the claim.
    const releaseRead = claimLocalReadMarker('thread-1', 900);
    const releaseUnread = claimLocalReadMarker('thread-1', 0);
    releaseRead();
    expect(pendingLocalReadMarker('thread-1')).toEqual({ held: true, lastReadAt: 0 });
    releaseUnread();
    expect(pendingLocalReadMarker('thread-1').held).toBe(false);
  });

  it('ignores a second release from the same caller', () => {
    const release = claimLocalReadMarker('thread-1', 0);
    const other = claimLocalReadMarker('thread-1', 0);
    release();
    release();
    expect(pendingLocalReadMarker('thread-1').held).toBe(true);
    other();
    expect(pendingLocalReadMarker('thread-1').held).toBe(false);
  });

  it('releases the claim when the write throws', async () => {
    await expect(withLocalReadMarker('thread-1', 0, async () => {
      throw new Error('nope');
    })).rejects.toThrow('nope');
    expect(pendingLocalReadMarker('thread-1').held).toBe(false);
  });

  it('holds the claim across the mark-unread RPC and its local patch', async () => {
    replaceAllThreads([mkThread('thread-1', 900)]);
    let duringRpc: ReturnType<typeof pendingLocalReadMarker> | undefined;
    setBindingMock('MarkThreadUnread', async () => {
      duringRpc = pendingLocalReadMarker('thread-1');
    });

    await markThreadUnread('thread-1');

    expect(duringRpc).toEqual({ held: true, lastReadAt: 0 });
    expect(getThreadById('thread-1')?.lastReadAt).toBe(0);
    expect(pendingLocalReadMarker('thread-1').held).toBe(false);
  });

  it('holds the claim at the stamp being persisted while mark-read is in flight', async () => {
    let duringRpc: ReturnType<typeof pendingLocalReadMarker> | undefined;
    setBindingMock('MarkThreadRead', async () => {
      duringRpc = pendingLocalReadMarker('thread-1');
    });

    await markThreadRead('thread-1', 1_234);

    expect(duringRpc).toEqual({ held: true, lastReadAt: 1_234 });
    expect(pendingLocalReadMarker('thread-1').held).toBe(false);
  });

  it('swallows a failed read stamp and still releases the claim', async () => {
    setBindingMock('MarkThreadRead', async () => {
      throw new Error('offline');
    });

    await expect(markThreadRead('thread-1', 1_234)).resolves.toBeUndefined();
    expect(pendingLocalReadMarker('thread-1').held).toBe(false);
  });
});
