import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  __sendsPendingOrderForTest,
  applySendsPendingFrame,
  beginSendsPendingRead,
  clearThreadStatus,
  endSendsPendingRead,
  getThreadStatus,
  hydrateSendsPending,
  projectThreadReverted,
  projectTurnStarted,
  resetForTest,
} from './threadStatuses.svelte';
import { markItemsFlushed, registerQueueItem, replaceQueueForThread, resetForTest as resetSendQueueForTest } from './sendQueue.svelte';
import { __setTransportHelloForTest } from './transportStatus.svelte';
import { __resetEntityIndexForTest, noteThread } from '../transport/entityIndex';
import { setBindingMock } from '../../test/mocks/bindings-app';
import type { TransportHello } from '../transport/wsClient';

const hello = (capabilities: string[], launchId = 'launch-1', backendId = 'gpu'): TransportHello => ({
  protocolVersion: 1, capabilities, backendId, backendName: backendId, serverTimeMs: 0,
  clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0, launchId,
});

// A provider:sends_pending frame for 'remote' from gpu at `sequence`.
const frame = (pending: boolean, sequence: number) => applySendsPendingFrame({ threadId: 'remote', pending }, 'gpu', sequence);
// A gap marker on the channel: no payload, and the sequence it lost through.
const gap = (sequence: number) => applySendsPendingFrame(null, 'gpu', sequence);

// The thread lives on another computer; this client has no pane on it.
// What it holds for the thread's queue comes from the wildcard queue
// frames, and the echo that would empty it never reaches it.
function strandFlushedEntry(threadId: string): void {
  markItemsFlushed(threadId, [{ queueItemId: 'queue:1', userItemId: 'user:1:flush:1', message: 'next' }]);
}

beforeEach(() => {
  resetForTest();
  resetSendQueueForTest();
  __resetEntityIndexForTest();
  noteThread('remote', 'gpu', 1);
});
afterEach(() => {
  __setTransportHelloForTest(null, 'gpu');
});

describe('a backend that publishes provider:sends_pending', () => {
  beforeEach(() => __setTransportHelloForTest(hello(['sends-pending.v1']), 'gpu'));

  it('reads the running bridge from the backend, not a queue mirror the echo never empties', () => {
    strandFlushedEntry('remote');
    expect(getThreadStatus('remote')).toBe('idle');

    frame(true, 1);
    expect(getThreadStatus('remote')).toBe('running');
    frame(false, 2);
    expect(getThreadStatus('remote')).toBe('idle');
  });

  it('keeps an active turn running whatever the bridge says', () => {
    projectTurnStarted('remote', 'round-1', 0, 1);
    frame(false, 1);
    expect(getThreadStatus('remote')).toBe('running');
  });

  it("drops a frame from a computer that does not own the thread", () => {
    applySendsPendingFrame({ threadId: 'remote', pending: true }, 'laptop', 1);
    expect(getThreadStatus('remote')).toBe('idle');
  });

  it('applies an unsequenced frame as it comes', () => {
    applySendsPendingFrame({ threadId: 'remote', pending: true }, 'gpu', undefined);
    expect(getThreadStatus('remote')).toBe('running');
  });

  describe("this client's own queue RPC", () => {
    const options = (sendId: string) => ({ sendId }) as Parameters<typeof registerQueueItem>[2];
    const wire = (sendId: string, sendsPending?: { pending: boolean; sequence: number }) => (
      { id: `queue:${sendId}`, sendId, threadId: 'remote', message: 'next', enqueuedAt: 1, attachmentIds: [], sendsPending }
    );
    function holdRegistration(): { queued: Promise<unknown>; reply: (item: unknown) => void } {
      let reply!: (item: unknown) => void;
      setBindingMock('RegisterQueueItem', () => new Promise((resolve) => { reply = resolve; }));
      return { queued: registerQueueItem('remote', 'next', options('send-1')), reply };
    }

    it('is running until it answers, and its answer outranks the frames written before it', async () => {
      frame(false, 3);
      const { queued, reply } = holdRegistration();
      expect(getThreadStatus('remote')).toBe('running');
      reply(wire('send-1', { pending: true, sequence: 7 }));
      await queued;
      expect(getThreadStatus('remote')).toBe('running');
      // Frames batched before the reply was written arrive after it.
      frame(false, 6);
      expect(getThreadStatus('remote')).toBe('running');
      frame(true, 7);
      expect(getThreadStatus('remote')).toBe('running');
      frame(false, 8);
      expect(getThreadStatus('remote')).toBe('idle');
    });

    it('stays running when a queue frame takes its provisional entry before the reply', async () => {
      const { queued, reply } = holdRegistration();
      replaceQueueForThread('remote', [{ id: 'queue:send-1', threadId: 'remote', message: 'next', enqueuedAt: 1, sendId: 'send-1', attachmentIds: [] }]);
      replaceQueueForThread('remote', []);
      markItemsFlushed('remote', [{ queueItemId: 'queue:send-1', userItemId: 'user:1', message: 'next', sendId: 'send-1' }]);
      expect(getThreadStatus('remote')).toBe('running');
      reply(wire('send-1', { pending: true, sequence: 2 }));
      await queued;
      expect(getThreadStatus('remote')).toBe('running');
    });

    it('orders concurrent answers by sequence, whichever reply arrives first', async () => {
      const replies: ((item: unknown) => void)[] = [];
      setBindingMock('RegisterQueueItem', () => new Promise((resolve) => { replies.push(resolve); }));
      frame(false, 4);
      const older = registerQueueItem('remote', 'next', options('send-1'));
      const newer = registerQueueItem('remote', 'later', options('send-2'));
      replies[1](wire('send-2', { pending: true, sequence: 6 }));
      await newer;
      replies[0](wire('send-1', { pending: false, sequence: 5 }));
      await older;
      expect(getThreadStatus('remote')).toBe('running');
    });

    it('drops an answer the stream already passed', async () => {
      const { queued, reply } = holdRegistration();
      frame(true, 5);
      frame(false, 6);
      reply(wire('send-1', { pending: true, sequence: 5 }));
      await queued;
      expect(getThreadStatus('remote')).toBe('idle');
    });

    it("counts the sequence of a moved thread's frame, which can carry a loss announcement", async () => {
      noteThread('moved', 'laptop', 2);
      frame(true, 4);
      const { queued, reply } = holdRegistration();
      // The clear at sequence 6 was lost; the announcement rides a frame for
      // a thread this client now attributes to another computer.
      applySendsPendingFrame({ threadId: 'moved', pending: true }, 'gpu', 7);
      expect(getThreadStatus('moved')).toBe('idle');
      const read = beginSendsPendingRead();
      hydrateSendsPending('remote', false, read);
      endSendsPendingRead(read);
      reply(wire('send-1', { pending: true, sequence: 5 }));
      await queued;
      expect(getThreadStatus('remote')).toBe('idle');
    });

    it('drops an answer that arrives after a lost frame it predates and the snapshot that repaired it', async () => {
      frame(true, 4);
      const { queued, reply } = holdRegistration();
      // The clear at sequence 6 was lost; the gap marker covers it, and the
      // snapshot read it set off reports the thread idle.
      gap(6);
      const read = beginSendsPendingRead();
      hydrateSendsPending('remote', false, read);
      endSendsPendingRead(read);
      reply(wire('send-1', { pending: true, sequence: 5 }));
      await queued;
      expect(getThreadStatus('remote')).toBe('idle');
    });

    it('drops the answer of a computer the thread moved away from during the call', async () => {
      __setTransportHelloForTest(hello(['sends-pending.v1'], 'laptop-launch', 'laptop'), 'laptop');
      try {
        const { queued, reply } = holdRegistration();
        noteThread('remote', 'laptop', 2);
        reply(wire('send-1', { pending: true, sequence: 9 }));
        await queued;
        expect(getThreadStatus('remote')).toBe('idle');
        expect(__sendsPendingOrderForTest().ahead).toBe(0);
      } finally {
        __setTransportHelloForTest(null, 'laptop');
      }
    });

    it('orders against the launch the backend runs now', async () => {
      frame(false, 40);
      __setTransportHelloForTest(hello(['sends-pending.v1'], 'launch-2'), 'gpu');
      const { queued, reply } = holdRegistration();
      reply(wire('send-1', { pending: true, sequence: 1 }));
      await queued;
      expect(getThreadStatus('remote')).toBe('running');
    });

    it('does not hold a thread whose registration failed', async () => {
      setBindingMock('RegisterQueueItem', async () => { throw new Error('queue full'); });
      await expect(registerQueueItem('remote', 'next', options('send-1'))).rejects.toThrow('queue full');
      expect(getThreadStatus('remote')).toBe('idle');
    });

    it('leaves the status to frames when the backend answers without sends pending', async () => {
      setBindingMock('RegisterQueueItem', async () => wire('send-1'));
      await registerQueueItem('remote', 'next', options('send-1'));
      expect(getThreadStatus('remote')).toBe('idle');
      expect(__sendsPendingOrderForTest().ahead).toBe(0);
    });

    it('outranks a snapshot read in flight, which may predate the registration', async () => {
      setBindingMock('RegisterQueueItem', async () => wire('send-1', { pending: true, sequence: 7 }));
      const read = beginSendsPendingRead();
      await registerQueueItem('remote', 'next', options('send-1'));
      hydrateSendsPending('remote', false, read);
      endSendsPendingRead(read);
      expect(getThreadStatus('remote')).toBe('running');
    });

    it('retains an answer only until the stream reaches it, the thread is cleared, or its computer detaches', async () => {
      setBindingMock('RegisterQueueItem', async () => wire('send-1', { pending: true, sequence: 7 }));
      await registerQueueItem('remote', 'next', options('send-1'));
      expect(__sendsPendingOrderForTest().ahead).toBe(1);
      applySendsPendingFrame({ threadId: 'other', pending: false }, 'gpu', 7);
      expect(__sendsPendingOrderForTest().ahead).toBe(0);

      await registerQueueItem('remote', 'next', options('send-2'));
      setBindingMock('RegisterQueueItem', async () => wire('send-3', { pending: true, sequence: 8 }));
      await registerQueueItem('remote', 'next', options('send-3'));
      clearThreadStatus('remote');
      expect(__sendsPendingOrderForTest().ahead).toBe(0);
      // A frame for a cleared thread retains nothing for it.
      frame(false, 9);
      expect(__sendsPendingOrderForTest().ahead).toBe(0);
    });
  });

  it('lets a push that landed during a snapshot read win over it', () => {
    const crossed = beginSendsPendingRead();
    frame(false, 1);
    hydrateSendsPending('remote', true, crossed);
    endSendsPendingRead(crossed);
    expect(getThreadStatus('remote')).toBe('idle');

    const clean = beginSendsPendingRead();
    hydrateSendsPending('remote', true, clean);
    endSendsPendingRead(clean);
    expect(getThreadStatus('remote')).toBe('running');
  });

  it('clears on a revert and keeps an older snapshot from putting it back', () => {
    frame(true, 1);
    const read = beginSendsPendingRead();
    projectThreadReverted('remote');
    expect(getThreadStatus('remote')).toBe('idle');
    hydrateSendsPending('remote', true, read);
    endSendsPendingRead(read);
    expect(getThreadStatus('remote')).toBe('idle');
  });

  it("drops the former computer's answer when the thread moves, and keeps it for a new epoch on the same computer", () => {
    frame(true, 5);
    noteThread('remote', 'gpu', 2);
    expect(getThreadStatus('remote')).toBe('running');

    const read = beginSendsPendingRead();
    noteThread('remote', 'laptop', 3);
    __setTransportHelloForTest(hello(['sends-pending.v1'], 'laptop-launch', 'laptop'), 'laptop');
    try {
      expect(getThreadStatus('remote')).toBe('idle');
      // A read of the former owner that was in flight cannot restore it.
      hydrateSendsPending('remote', true, read);
      endSendsPendingRead(read);
      expect(getThreadStatus('remote')).toBe('idle');
      // Nor can a late frame of the former owner.
      frame(true, 6);
      expect(getThreadStatus('remote')).toBe('idle');
      applySendsPendingFrame({ threadId: 'remote', pending: true }, 'laptop', 1);
      expect(getThreadStatus('remote')).toBe('running');
    } finally {
      __setTransportHelloForTest(null, 'laptop');
    }
  });
});

describe('a backend that predates provider:sends_pending', () => {
  beforeEach(() => __setTransportHelloForTest(hello([]), 'gpu'));

  it('keeps reading the queue mirror', () => {
    strandFlushedEntry('remote');
    expect(getThreadStatus('remote')).toBe('running');
  });
});
