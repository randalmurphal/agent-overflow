import { flushSync } from 'svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import {
  __flushSettleOrderForTest,
  __resetFlushSettleOrderForTest,
  applyQueueFlushed,
  applySendsPendingEvent,
} from './eventsQueue';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { buildPane } from '../../test/helpers/chat';
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

const hello = (launchId: string, backendId = 'gpu'): TransportHello => ({
  protocolVersion: 1, capabilities: ['sends-pending.v1'], backendId, backendName: backendId, serverTimeMs: 0,
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

const held = (threadId = 'remote'): string[] => getFlushedForThread(threadId).map((item) => item.message);

describe('Zone 2 entries of a thread with no pane', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetSendQueueForTest();
    resetThreadStatuses();
    __resetFlushSettleOrderForTest();
    __resetEntityIndexForTest();
    noteThread('remote', 'gpu', 1);
    noteThread('other', 'gpu', 1);
    __setTransportHelloForTest(hello('launch-1'), 'gpu');
    setCarriedSessionScopes('gpu', ['threads:read', 'threads:operate']);
  });
  afterEach(() => {
    resetSendQueueForTest();
    resetThreadStatuses();
    __resetFlushSettleOrderForTest();
    __setTransportHelloForTest(null, 'gpu');
    __setTransportHelloForTest(null, 'laptop');
    __resetScopesForTest();
  });

  it('leave when a frame reads nothing pending through their flush', () => {
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

  it('stay while the thread still has a send pending', () => {
    flushed(1, 3);
    applySendsPendingEvent({ threadId: 'remote', pending: true, flushedThrough: 3 }, 'gpu', 100);

    expect(held()).toEqual(['message 1']);
  });

  it('leave when the settle is replayed ahead of the flush frames it covers', () => {
    settled(5);
    expect(__flushSettleOrderForTest().ahead).toBe(1);

    flushed(1, 4);
    flushed(2, 5);
    expect(held()).toEqual([]);
    // The stream reached the settle: nothing more can arrive under it.
    expect(__flushSettleOrderForTest().ahead).toBe(0);

    flushed(3, 6);
    expect(held()).toEqual(['message 3']);
  });

  it('retain no settle the flush stream already reached', () => {
    flushed(1, 5, 'other');
    flushed(2, 6);
    settled(6);

    expect(held()).toEqual([]);
    expect(__flushSettleOrderForTest().ahead).toBe(0);
  });

  it('retain no settle for a thread this client is sent no flush frames for', () => {
    setCarriedSessionScopes('gpu', ['threads:read']);
    settled(5);

    expect(__flushSettleOrderForTest().ahead).toBe(0);
  });

  it('forget a settle ahead once a gap moves the flush stream past it', () => {
    settled(7);
    expect(__flushSettleOrderForTest().ahead).toBe(1);

    applyQueueFlushed(null, 'gpu', 9);
    expect(__flushSettleOrderForTest().ahead).toBe(0);
  });

  it('retain nothing ordered against a computer that detached', () => {
    stageBackend({ id: 'gpu', hello: hello('launch-1') });
    setCarriedSessionScopes('gpu', ['threads:read', 'threads:operate']);
    noteThread('remote', 'gpu', 1);
    try {
      flushed(1, 3, 'other');
      settled(9);
      expect(__flushSettleOrderForTest()).toEqual({ streams: 1, ahead: 1 });
    } finally {
      detachBackend('gpu');
      resetStagedBackends();
    }

    expect(__flushSettleOrderForTest()).toEqual({ streams: 0, ahead: 0 });
  });

  it('leave on a frame the status ordering drops as older than a queue answer', async () => {
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

  it("stay on this client's queue answer, which names no flush", async () => {
    flushed(1, 3);
    setBindingMock('RegisterQueueItem', async () => ({
      id: 'queue:send-1', sendId: 'send-1', threadId: 'remote', message: 'next', enqueuedAt: 1, attachmentIds: [],
      sendsPending: { pending: false, sequence: 8 },
    }));
    await registerQueueItem('remote', 'next', { sendId: 'send-1' } as Parameters<typeof registerQueueItem>[2]);

    expect(held()).toEqual(['message 1']);
  });

  it('stay for a backend that does not report what its answer covers', () => {
    flushed(1, 3);
    settled(undefined);

    expect(held()).toEqual(['message 1']);
  });

  it('stay when their flush frame carried no sequence', () => {
    flushed(1, undefined);
    settled(50);

    expect(held()).toEqual(['message 1']);
  });

  it('stay on a frame from a computer that does not own the thread', () => {
    __setTransportHelloForTest(hello('laptop-launch', 'laptop'), 'laptop');
    flushed(1, 3);
    settled(50, 100, 'remote', 'laptop');

    expect(held()).toEqual(['message 1']);
  });

  it('leave when a later launch reads nothing pending, whatever its sequence', () => {
    flushed(1, 40);
    __setTransportHelloForTest(hello('launch-2'), 'gpu');

    settled(0);
    expect(held()).toEqual([]);
  });

  it("stay when the settle ahead belongs to an earlier launch's stream", () => {
    settled(50);
    __setTransportHelloForTest(hello('launch-2'), 'gpu');

    flushed(1, 3);
    expect(held()).toEqual(['message 1']);
    expect(__flushSettleOrderForTest().ahead).toBe(0);
  });

  it('keep the mark of their own frame across a live-state read', () => {
    flushed(1, 3);
    const snapshot: FlushedItem[] = [1, 2].map((n) => ({ ...entry(n), flushedAt: 0 }));
    replaceFlushedForThread('remote', snapshot, flushMarkAt('gpu', 9));

    expect(getFlushedForThread('remote').map((item) => item.flushMark?.sequence)).toEqual([3, 9]);
    settled(3);
    expect(held()).toEqual(['message 2']);
    settled(9);
    expect(held()).toEqual([]);
  });

  it('take the read position when their mark is from an earlier launch', () => {
    flushed(1, 40);
    __setTransportHelloForTest(hello('launch-2'), 'gpu');
    replaceFlushedForThread('remote', [{ ...entry(1), flushedAt: 0 }], flushMarkAt('gpu', 2));

    expect(getFlushedForThread('remote')[0].flushMark).toEqual({ backend: 'gpu', launchId: 'launch-2', sequence: 2 });
  });

  it('stay with a mounted pane, which holds each until it renders the row', async () => {
    const pane = await buildPane();
    const threadId = pane.threadId!;
    markItemsFlushed(threadId, [entry(1)], flushMarkAt('gpu', 3));
    noteThread(threadId, 'gpu', 1);

    settled(3, 100, threadId);
    expect(held(threadId)).toEqual(['message 1']);
  });
});
