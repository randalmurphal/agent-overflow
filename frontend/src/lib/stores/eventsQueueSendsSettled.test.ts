import { flushSync } from 'svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import './eventsQueue';
import { buildPane } from '../../test/helpers/chat';
import { resetBindingMocks } from '../../test/mocks/bindings-app';
import { probeReactivity } from '../../test/helpers/reactivity.svelte';
import {
  getFlushedForThread,
  getQueueRevisionForThread,
  markItemsFlushed,
  resetForTest as resetSendQueueForTest,
} from './sendQueue.svelte';
import {
  applySendsPendingFrame,
  beginSendsPendingRead,
  endSendsPendingRead,
  hydrateSendsPending,
  resetForTest as resetThreadStatuses,
} from './threadStatuses.svelte';
import { __resetEntityIndexForTest, noteThread } from '../transport/entityIndex';

function flush(threadId: string): void {
  markItemsFlushed(threadId, [
    { queueItemId: 'queue:1', userItemId: 'user:1:flush:1', message: 'first' },
    { queueItemId: 'queue:2', userItemId: 'user:2:flush:1', message: 'second' },
  ]);
}

describe('Zone 2 when the backend reports nothing pending', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetSendQueueForTest();
    resetThreadStatuses();
    __resetEntityIndexForTest();
    noteThread('remote', 'gpu', 1);
  });
  afterEach(() => {
    resetSendQueueForTest();
    resetThreadStatuses();
  });

  it('releases the entries of a thread with no pane on a frame', () => {
    flush('remote');
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
    flush('remote');
    const read = beginSendsPendingRead();
    hydrateSendsPending('remote', false, read);
    endSendsPendingRead(read);

    expect(getFlushedForThread('remote')).toHaveLength(0);
  });

  it('ignores a frame from a computer that does not own the thread', () => {
    flush('remote');
    applySendsPendingFrame({ threadId: 'remote', pending: true }, 'gpu', 5);
    applySendsPendingFrame({ threadId: 'remote', pending: false }, 'laptop', 6);

    expect(getFlushedForThread('remote')).toHaveLength(2);
  });

  it('leaves the entries of a mounted thread to its pane', async () => {
    const pane = await buildPane();
    const threadId = pane.threadId!;
    flush(threadId);
    const read = beginSendsPendingRead();
    hydrateSendsPending(threadId, false, read);
    endSendsPendingRead(read);

    expect(getFlushedForThread(threadId)).toHaveLength(2);
  });
});
