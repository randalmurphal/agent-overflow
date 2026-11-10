import { flushSync } from 'svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { applyQueueFlushed } from './eventsQueue';
import { buildPane } from '../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { probeReactivity } from '../../test/helpers/reactivity.svelte';
import {
  getFlushedForThread,
  getQueueRevisionForThread,
  markItemsFlushed,
  registerQueueItem,
  resetForTest as resetSendQueueForTest,
} from './sendQueue.svelte';
import {
  applySendsPendingFrame,
  beginSendsPendingRead,
  endSendsPendingRead,
  getThreadStatus,
  hydrateSendsPending,
  resetForTest as resetThreadStatuses,
} from './threadStatuses.svelte';
import { __setTransportHelloForTest } from './transportStatus.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { __resetEntityIndexForTest, noteThread } from '../transport/entityIndex';
import type { TransportHello } from '../transport/wsClient';

const hello = (capabilities: string[], backendId: string): TransportHello => ({
  protocolVersion: 1, capabilities, backendId, backendName: backendId, serverTimeMs: 0,
  clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0, launchId: 'launch-1',
});

const ENTRIES = [
  { queueItemId: 'queue:1', userItemId: 'user:1:flush:1', message: 'first' },
  { queueItemId: 'queue:2', userItemId: 'user:2:flush:1', message: 'second' },
];

// A snapshot row for `threadId`, read while `during` runs.
function snapshotRow(threadId: string, pending: boolean, during: () => void = () => {}): void {
  const read = beginSendsPendingRead();
  during();
  endSendsPendingRead(read);
  hydrateSendsPending(threadId, pending, read);
}

describe('Zone 2 when the backend reports nothing pending', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetSendQueueForTest();
    resetThreadStatuses();
    __resetEntityIndexForTest();
    noteThread('remote', 'gpu', 1);
    __setTransportHelloForTest(hello(['sends-pending.v1'], 'gpu'), 'gpu');
    __setTransportHelloForTest(hello(['sends-pending.v1'], 'home'), HOME_BACKEND);
  });
  afterEach(() => {
    resetSendQueueForTest();
    resetThreadStatuses();
    __setTransportHelloForTest(null, 'gpu');
    __setTransportHelloForTest(null, HOME_BACKEND);
  });

  it('releases the entries of a thread with no pane on a frame', () => {
    markItemsFlushed('remote', ENTRIES);
    const reader = probeReactivity(() => getFlushedForThread('remote').length);
    const revision = getQueueRevisionForThread('remote');

    applySendsPendingFrame({ threadId: 'remote', pending: true }, 'gpu', 1);
    expect(getFlushedForThread('remote')).toHaveLength(2);

    applySendsPendingFrame({ threadId: 'remote', pending: false }, 'gpu', 2);
    flushSync();
    expect(getFlushedForThread('remote')).toHaveLength(0);
    expect(reader.latest).toBe(0);
    expect(getQueueRevisionForThread('remote')).toBeGreaterThan(revision);
    reader.dispose();
  });

  it('releases them on a snapshot row', () => {
    markItemsFlushed('remote', ENTRIES);
    snapshotRow('remote', false);

    expect(getFlushedForThread('remote')).toHaveLength(0);
  });

  it('keeps an entry flushed while the snapshot was being read', () => {
    applySendsPendingFrame({ threadId: 'remote', pending: true }, 'gpu', 1);
    snapshotRow('remote', false, () => {
      applyQueueFlushed({ threadId: 'remote', items: ENTRIES });
    });

    // The row still settles the status; only the entries are kept.
    expect(getThreadStatus('remote')).toBe('idle');
    expect(getFlushedForThread('remote')).toHaveLength(2);
  });

  it("keeps them on this client's queue RPC answer", async () => {
    markItemsFlushed('remote', ENTRIES);
    setBindingMock('RegisterQueueItem', async () => ({
      id: 'queue:send-1', sendId: 'send-1', threadId: 'remote', message: 'next', enqueuedAt: 1, attachmentIds: [],
      sendsPending: { pending: false, sequence: 4 },
    }));
    await registerQueueItem('remote', 'next', { sendId: 'send-1' } as Parameters<typeof registerQueueItem>[2]);

    expect(getFlushedForThread('remote').map((entry) => entry.userItemId)).toEqual(ENTRIES.map((entry) => entry.userItemId));
  });

  it('keeps them for a backend that does not publish the answer', () => {
    __setTransportHelloForTest(hello([], 'gpu'), 'gpu');
    markItemsFlushed('remote', ENTRIES);
    // The snapshot row of such a backend omits the field, which reads false.
    snapshotRow('remote', false);

    expect(getFlushedForThread('remote')).toHaveLength(2);
    expect(getThreadStatus('remote')).toBe('running');
  });

  it('ignores a frame from a computer that does not own the thread', () => {
    markItemsFlushed('remote', ENTRIES);
    applySendsPendingFrame({ threadId: 'remote', pending: true }, 'gpu', 5);
    applySendsPendingFrame({ threadId: 'remote', pending: false }, 'laptop', 6);

    expect(getFlushedForThread('remote')).toHaveLength(2);
  });

  it('leaves the entries of a mounted thread to its pane', async () => {
    const pane = await buildPane();
    const threadId = pane.threadId!;
    markItemsFlushed(threadId, ENTRIES);
    snapshotRow(threadId, false);

    expect(getFlushedForThread(threadId)).toHaveLength(2);
  });
});
