import { flushSync } from 'svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { applyQueueFlushed, applySendsPendingEvent } from './eventsQueue';
import {
  __flushSettlementFloorsForTest,
  __resetFlushSettlementForTest,
  reconcileSettledFlushes,
} from './flushSettlement';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { buildPane, makeThread } from '../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { probeReactivity } from '../../test/helpers/reactivity.svelte';
import {
  flushMarkAt,
  getFlushedForThread,
  getQueueRevisionForThread,
  markItemsFlushed,
  registerQueueItem,
  replaceFlushedForThread,
  resetForTest as resetSendQueueForTest,
  type FlushedItem,
} from './sendQueue.svelte';
import { getThreadStatus, resetForTest as resetThreadStatuses } from './threadStatuses.svelte';
import { __setTransportHelloForTest } from './transportStatus.svelte';
import { detachBackend } from '../transport/backends';
import { __resetEntityIndexForTest, noteThread } from '../transport/entityIndex';
import { __resetScopesForTest, setCarriedSessionScopes } from '../transport/scopes';
import type { TransportHello } from '../transport/wsClient';

const hello = (launchId: string, backendId = 'gpu', capabilities = ['sends-pending.v1', 'flush-settlement.v1']): TransportHello => ({
  protocolVersion: 1, capabilities, backendId, backendName: backendId, serverTimeMs: 0,
  clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0, launchId,
});

const entry = (n: number) => ({ queueItemId: `queue:${n}`, userItemId: `user:${n}:flush:1`, message: `message ${n}` });

// A provider:queue_flushed frame for `threadId` at `sequence`, carrying entry `n`.
function flushed(n: number, sequence: number | undefined, threadId = 'remote'): void {
  applyQueueFlushed({ threadId, items: [entry(n)] }, 'gpu', sequence);
}

// A provider:sends_pending frame that reads nothing pending through `flushedThrough`.
function settled(flushedThrough: number | undefined, sequence = 100, threadId = 'remote', backend = 'gpu'): void {
  applySendsPendingEvent({ threadId, pending: false, flushedThrough }, backend, sequence);
}

// The computer's answer: every thread but `unsettled` is settled through `flushedThrough`.
function answers(flushedThrough: number, unsettled: string[] = []) {
  return setBindingMock('ListUnsettledFlushes', async () => ({ flushedThrough, unsettledThreadIds: unsettled }));
}

const held = (threadId = 'remote'): string[] => getFlushedForThread(threadId).map((item) => item.message);

beforeEach(() => {
  resetBindingMocks();
  resetSendQueueForTest();
  resetThreadStatuses();
  __resetFlushSettlementForTest();
  __resetEntityIndexForTest();
  noteThread('remote', 'gpu', 1);
  noteThread('other', 'gpu', 1);
  __setTransportHelloForTest(hello('launch-1'), 'gpu');
  setCarriedSessionScopes('gpu', ['threads:read', 'threads:operate']);
});
afterEach(() => {
  resetSendQueueForTest();
  resetThreadStatuses();
  __resetFlushSettlementForTest();
  __setTransportHelloForTest(null, 'gpu');
  __setTransportHelloForTest(null, 'laptop');
  __resetScopesForTest();
});

describe('a frame that reads nothing pending, for a thread with no pane', () => {
  it('releases the entries flushed up to the sequence it names', () => {
    flushed(1, 3);
    flushed(2, 5);
    const reader = probeReactivity(() => getFlushedForThread('remote').length);
    const revision = getQueueRevisionForThread('remote');

    // Read before the second flush: it settles only the first.
    settled(3);
    expect(held()).toEqual(['message 2']);

    settled(5);
    flushSync();
    expect(held()).toEqual([]);
    expect(reader.latest).toBe(0);
    expect(getQueueRevisionForThread('remote')).toBeGreaterThan(revision);
    reader.dispose();
  });

  it('releases nothing while the thread still has a send pending', () => {
    flushed(1, 3);
    applySendsPendingEvent({ threadId: 'remote', pending: true, flushedThrough: 3 }, 'gpu', 100);

    expect(held()).toEqual(['message 1']);
  });

  it('releases even when the status ordering drops it as older than a queue answer', async () => {
    flushed(1, 3);
    setBindingMock('RegisterQueueItem', async () => ({
      id: 'queue:send-1', sendId: 'send-1', threadId: 'remote', message: 'next', enqueuedAt: 1, attachmentIds: [],
      sendsPending: { pending: true, sequence: 8 },
    }));
    await registerQueueItem('remote', 'next', { sendId: 'send-1' } as Parameters<typeof registerQueueItem>[2]);

    settled(3, 7);
    expect(held()).toEqual([]);
    expect(getThreadStatus('remote')).toBe('running');
  });

  it("is not stood in for by this client's queue answer, which names no flush", async () => {
    flushed(1, 3);
    setBindingMock('RegisterQueueItem', async () => ({
      id: 'queue:send-1', sendId: 'send-1', threadId: 'remote', message: 'next', enqueuedAt: 1, attachmentIds: [],
      sendsPending: { pending: false, sequence: 8 },
    }));
    await registerQueueItem('remote', 'next', { sendId: 'send-1' } as Parameters<typeof registerQueueItem>[2]);

    expect(held()).toEqual(['message 1']);
  });

  it('releases nothing from a backend that does not say what its answer covers', () => {
    flushed(1, 3);
    settled(undefined);

    expect(held()).toEqual(['message 1']);
  });

  it('releases no entry whose flush frame carried no sequence', () => {
    flushed(1, undefined);
    settled(50);

    expect(held()).toEqual(['message 1']);
  });

  it('releases nothing when it comes from a computer that does not own the thread', () => {
    __setTransportHelloForTest(hello('laptop-launch', 'laptop'), 'laptop');
    flushed(1, 3);
    settled(50, 100, 'remote', 'laptop');

    expect(held()).toEqual(['message 1']);
  });

  it('releases the entries of an earlier launch, whatever its sequence', () => {
    flushed(1, 40);
    __setTransportHelloForTest(hello('launch-2'), 'gpu');

    settled(0);
    expect(held()).toEqual([]);
  });
});

describe('the position an entry is settled by', () => {
  it('is the flush frame for an entry an echo added first', () => {
    // The canonical echo beat the frame: the entry exists with no position.
    markItemsFlushed('remote', [entry(1)]);
    const revision = getQueueRevisionForThread('remote');
    flushed(1, 3);

    expect(getFlushedForThread('remote').map((item) => item.flushMark?.sequence)).toEqual([3]);
    expect(getQueueRevisionForThread('remote')).toBeGreaterThan(revision);
    settled(3);
    expect(held()).toEqual([]);
  });

  it('is the newest flush frame for a send flushed again under the same id', () => {
    flushed(1, 3);
    flushed(1, 7);

    settled(3);
    expect(held()).toEqual(['message 1']);
    settled(7);
    expect(held()).toEqual([]);
  });

  it('is left alone by a replay of the same frame', () => {
    flushed(1, 3);
    const revision = getQueueRevisionForThread('remote');
    flushed(1, 3);

    expect(getQueueRevisionForThread('remote')).toBe(revision);
  });

  it('survives a live-state read, which marks only the entries it adds', () => {
    flushed(1, 3);
    const snapshot: FlushedItem[] = [1, 2].map((n) => ({ ...entry(n), flushedAt: 0 }));
    replaceFlushedForThread('remote', snapshot, flushMarkAt('gpu', 9));

    expect(getFlushedForThread('remote').map((item) => item.flushMark?.sequence)).toEqual([3, 9]);
    settled(3);
    expect(held()).toEqual(['message 2']);
    settled(9);
    expect(held()).toEqual([]);
  });

  it("is the live-state read's when the entry's own is from an earlier launch", () => {
    flushed(1, 40);
    __setTransportHelloForTest(hello('launch-2'), 'gpu');
    replaceFlushedForThread('remote', [{ ...entry(1), flushedAt: 0 }], flushMarkAt('gpu', 2));

    expect(getFlushedForThread('remote')[0].flushMark).toEqual({ backend: 'gpu', launchId: 'launch-2', sequence: 2 });
  });
});

describe('a thread with a mounted pane', () => {
  it('keeps its entries on a settle and releases them when the pane leaves', async () => {
    const pane = await buildPane();
    const threadId = pane.threadId!;
    noteThread(threadId, 'gpu', 1);
    flushed(1, 3, threadId);
    settled(3, 100, threadId);
    // Flushed after the settle: not covered by it.
    flushed(2, 5, threadId);
    expect(held(threadId)).toEqual(['message 1', 'message 2']);

    await pane.switchThread(makeThread({ id: 'elsewhere' }));
    expect(held(threadId)).toEqual(['message 2']);
  });

  it('keeps entries marked under a later launch when it leaves', async () => {
    const pane = await buildPane();
    const threadId = pane.threadId!;
    noteThread(threadId, 'gpu', 1);
    flushed(1, 30, threadId);
    settled(30, 100, threadId);
    __setTransportHelloForTest(hello('launch-2'), 'gpu');
    // The same message is pending again under the new launch.
    replaceFlushedForThread(threadId, [{ ...entry(1), flushedAt: 0 }], flushMarkAt('gpu', 2));

    await pane.switchThread(makeThread({ id: 'elsewhere' }));
    expect(held(threadId)).toEqual(['message 1']);
  });
});

describe("the computer's settlement answer", () => {
  it('releases every thread it does not name, however the frames were ordered', async () => {
    // A replay handed the settle over ahead of the flush frames it covers.
    settled(5);
    flushed(1, 4);
    flushed(2, 5, 'other');
    expect(held()).toEqual(['message 1']);
    const read = answers(5, ['other']);

    await reconcileSettledFlushes('gpu');

    expect(read).toHaveBeenCalledTimes(1);
    expect(held()).toEqual([]);
    expect(held('other')).toEqual(['message 2']);
  });

  it('settles a flush frame its reply overtook', async () => {
    answers(5, ['other']);
    await reconcileSettledFlushes('gpu');

    flushed(1, 5);
    flushed(2, 4, 'other');
    flushed(3, 6);
    expect(held()).toEqual(['message 3']);
    expect(held('other')).toEqual(['message 2']);
  });

  it("settles no flush frame of a later launch's stream", async () => {
    answers(50);
    await reconcileSettledFlushes('gpu');
    __setTransportHelloForTest(hello('launch-2'), 'gpu');

    flushed(1, 3);
    expect(held()).toEqual(['message 1']);
  });

  it('leaves a thread that moved to another computer', async () => {
    __setTransportHelloForTest(hello('laptop-launch', 'laptop'), 'laptop');
    flushed(1, 3);
    noteThread('remote', 'laptop', 2);
    applyQueueFlushed({ threadId: 'remote', items: [entry(2)] }, 'laptop', 1);
    answers(50);

    await reconcileSettledFlushes('gpu');
    expect(held()).toEqual(['message 1', 'message 2']);
  });

  it('is not replaced by an older answer that arrives after it', async () => {
    answers(9);
    await reconcileSettledFlushes('gpu');
    answers(5, ['remote']);
    await reconcileSettledFlushes('gpu');

    flushed(1, 7);
    expect(held()).toEqual([]);
  });

  it('leaves the entries another computer flushed', async () => {
    __setTransportHelloForTest(hello('laptop-launch', 'laptop'), 'laptop');
    // A thread this client has attributed to no computer yet.
    applyQueueFlushed({ threadId: 'laptop-thread', items: [entry(1)] }, 'laptop', 2);
    answers(50);

    await reconcileSettledFlushes('gpu');
    expect(held('laptop-thread')).toEqual(['message 1']);
  });

  it('leaves a mounted thread to its pane until the pane leaves', async () => {
    const pane = await buildPane();
    const threadId = pane.threadId!;
    noteThread(threadId, 'gpu', 1);
    flushed(1, 3, threadId);
    answers(3);

    await reconcileSettledFlushes('gpu');
    expect(held(threadId)).toEqual(['message 1']);

    await pane.switchThread(makeThread({ id: 'elsewhere' }));
    expect(held(threadId)).toEqual([]);
  });

  it('is dropped when the backend restarted during the read', async () => {
    flushed(1, 3);
    let answer!: (value: unknown) => void;
    setBindingMock('ListUnsettledFlushes', () => new Promise((resolve) => { answer = resolve; }));
    const read = reconcileSettledFlushes('gpu');
    __setTransportHelloForTest(hello('launch-2'), 'gpu');
    answer({ flushedThrough: 50, unsettledThreadIds: [] });
    await read;

    expect(held()).toEqual(['message 1']);
    expect(__flushSettlementFloorsForTest()).toBe(0);
  });

  it('is not read from a backend that does not report settlement', async () => {
    __setTransportHelloForTest(hello('launch-1', 'gpu', ['sends-pending.v1']), 'gpu');
    const read = answers(5);

    await reconcileSettledFlushes('gpu');
    expect(read).not.toHaveBeenCalled();
  });

  it('is not read by a session that cannot operate the threads', async () => {
    setCarriedSessionScopes('gpu', ['threads:read']);
    const read = answers(5);

    await reconcileSettledFlushes('gpu');
    expect(read).not.toHaveBeenCalled();
  });

  it('is forgotten when its computer detaches', async () => {
    stageBackend({ id: 'gpu', hello: hello('launch-1') });
    setCarriedSessionScopes('gpu', ['threads:read', 'threads:operate']);
    try {
      answers(5);
      await reconcileSettledFlushes('gpu');
      expect(__flushSettlementFloorsForTest()).toBe(1);
    } finally {
      detachBackend('gpu');
      resetStagedBackends();
    }

    expect(__flushSettlementFloorsForTest()).toBe(0);
  });
});
