import { beforeEach, describe, expect, it } from 'vitest';
import { applyQueueStateChanged } from './eventsQueue';
import { getQueueRevisionForThread, resetForTest as resetSendQueueForTest } from './sendQueue.svelte';

describe('provider:queue_state_changed', () => {
  beforeEach(() => {
    resetSendQueueForTest();
  });

  // A queue snapshot read before the frame left can reach this client after
  // it, and the frame is the newer state even when it matches what this
  // client holds: a fresh pane holds an empty queue, the snapshot still
  // names a message another screen has since removed, and the removal's
  // frame changes nothing here. The revision guard must still discard that
  // snapshot.
  it('supersedes a snapshot read in flight when it changes nothing held', () => {
    const atRequest = getQueueRevisionForThread('t1');
    applyQueueStateChanged({ threadId: 't1', items: [] });
    expect(getQueueRevisionForThread('t1')).not.toBe(atRequest);
  });
});
