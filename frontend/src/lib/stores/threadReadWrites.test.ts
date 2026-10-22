import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { beforeEach, describe, expect, it } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import {
  claimLocalReadMarker,
  pendingLocalReadMarker,
  resetLocalReadMarkersForTest,
  withLocalReadMarker,
} from './threadReadWrites';
import {
  MARK_THREADS_READ_LIMIT,
  getThreadById,
  markThreadRead,
  markThreadUnread,
  markThreadsRead,
  replaceAllThreads,
  replaceThread,
} from './threads.svelte';
import { noteThreadRowFrame } from './threadRowFrames';
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
  it('splits Mark All Read into calls the backend takes, and keeps the rows of a call that worked when another fails', async () => {
    const threads = Array.from({ length: MARK_THREADS_READ_LIMIT + 1 }, (_, i) => ({
      ...mkThread(`t${i}`, 0),
      latestTurnCompletedAt: 100,
    }));
    replaceAllThreads(threads);
    const calls: string[][] = [];
    setBindingMock('MarkThreadsRead', async (ids: string[]) => {
      calls.push(ids);
      if (calls.length === 1) throw new Error('first call failed');
      return ids.map((id) => ({ ...mkThread(id, 200), latestTurnCompletedAt: 100 }));
    });

    await expect(markThreadsRead(threads)).rejects.toThrow('first call failed');
    expect(calls.map((ids) => ids.length)).toEqual([MARK_THREADS_READ_LIMIT, 1]);
    expect(getThreadById(`t${MARK_THREADS_READ_LIMIT}`)?.lastReadAt).toBe(200);
    expect(getThreadById('t0')?.lastReadAt).toBe(0);
    expect(pendingLocalReadMarker('t0').held).toBe(false);
    expect(pendingLocalReadMarker(`t${MARK_THREADS_READ_LIMIT}`).held).toBe(false);
  });
  it('counts a pending attention flag as something Mark All Read reads, and claims past it', async () => {
    replaceAllThreads([
      { ...mkThread('flagged', 500), latestTurnCompletedAt: 100, attention: { kind: 'review', reason: 'r', at: Date.now() + 60_000 } },
      { ...mkThread('read', 500), latestTurnCompletedAt: 100, attention: { kind: 'review', reason: 'r', at: 400 } },
    ]);
    let claimed: ReturnType<typeof pendingLocalReadMarker> | undefined;
    const calls: string[][] = [];
    setBindingMock('MarkThreadsRead', async (ids: string[]) => {
      calls.push(ids);
      claimed = pendingLocalReadMarker('flagged');
      return [];
    });

    await markThreadsRead([getThreadById('flagged')!, getThreadById('read')!]);
    expect(calls).toEqual([['flagged']]);
    expect(claimed?.held).toBe(true);
    expect(claimed?.lastReadAt).toBe(getThreadById('flagged')!.attention!.at);
  });
  it('keeps a whole-row frame that landed during Mark All Read over the older reply', async () => {
    replaceAllThreads([
      { ...mkThread('framed', 0), latestTurnCompletedAt: 100 },
      { ...mkThread('quiet', 0), latestTurnCompletedAt: 100 },
    ]);
    setBindingMock('MarkThreadsRead', async (ids: string[]) => {
      // A frame for 'framed' arrives mid-call: marked unread again elsewhere.
      noteThreadRowFrame('framed');
      replaceThread({ ...mkThread('framed', 0), latestTurnCompletedAt: 300 });
      return ids.map((id) => ({ ...mkThread(id, 200), latestTurnCompletedAt: 100 }));
    });

    await markThreadsRead([getThreadById('framed')!, getThreadById('quiet')!]);
    expect(getThreadById('framed')?.lastReadAt).toBe(0);
    expect(getThreadById('framed')?.latestTurnCompletedAt).toBe(300);
    expect(getThreadById('quiet')?.lastReadAt).toBe(200);
  });
});

// The client splits Mark All Read at the backend's per-call limit; a limit
// that drifted from the store's would have the backend refuse every split
// call. The Go source is the reference.
describe('the Mark All Read split', () => {
  it('matches store.MarkThreadsReadLimit', () => {
    const source = readFileSync(
      resolve(dirname(fileURLToPath(import.meta.url)), '../../../../internal/store/threads.go'),
      'utf8',
    );
    const limit = /MarkThreadsReadLimit = (\d+)/.exec(source);
    expect(limit, 'store.MarkThreadsReadLimit is not declared in internal/store/threads.go any more').not.toBeNull();
    expect(MARK_THREADS_READ_LIMIT).toBe(Number(limit![1]));
  });
});
