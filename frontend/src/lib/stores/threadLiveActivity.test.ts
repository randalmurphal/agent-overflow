import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { reconcileThreadLiveActivity } from './threadLiveActivity';
import { getBindingMock, setBindingMock } from '../../test/mocks/bindings-app';
import { __resetEntityIndexForTest, noteThread, threadBackend } from '../transport/entityIndex';
import {
  applySendsPendingFrame,
  getActiveTurn,
  getThreadStatus,
  projectApprovalRequest,
  projectApprovalResolution,
  projectTurnStarted,
  resetForTest,
} from './threadStatuses.svelte';
import { applyCompactingState, isThreadCompacting } from './compactingState.svelte';
import { hasScope } from '../transport/scopes';
import { __setTransportHelloForTest } from './transportStatus.svelte';
import type { TransportHello } from '../transport/wsClient';
import { __attachBackendForTest, __resetBackendsForTest, detachBackend } from '../transport/backends';

// A provider:sends_pending frame from gpu, each one newer than the last.
let frameSeq = 0;
const push = (threadId: string, pending: boolean) => applySendsPendingFrame({ threadId, pending }, 'gpu', ++frameSeq);

vi.mock('../transport/scopes', async (original) => ({ ...await original<object>(), hasScope: vi.fn(() => true) }));

beforeEach(() => {
  resetForTest();
  __resetEntityIndexForTest();
  for (const id of ['gpu-running', 'gpu-blocked', 'gpu-stale']) noteThread(id, 'gpu');
  noteThread('laptop-running', 'laptop');
});
afterEach(() => { vi.mocked(hasScope).mockReset().mockReturnValue(true); });

it('projects every named thread and clears the unnamed threads of that computer only', async () => {
  // Stale state from before a reconnect: the turn on gpu-stale ended while
  // this client was away, and the approval on gpu-blocked was answered.
  projectTurnStarted('gpu-stale', 'round-old', 2, 50);
  projectApprovalRequest('gpu-blocked', 'answered');
  // Another computer's thread is not this snapshot's to clear.
  projectTurnStarted('laptop-running', 'laptop-round', 1, 10);
  const read = setBindingMock('ListThreadLiveActivity', async () => [
    { threadId: 'gpu-running', activeTurn: { threadId: 'gpu-running', turnId: 'round-7', turnIndex: 7, startedAt: 700 }, approvalRequestIds: [], userInputRequestIds: [] },
    { threadId: 'gpu-blocked', approvalRequestIds: [], userInputRequestIds: ['question-1'], compactingSinceUnixMs: 0 },
    { threadId: 'gpu-compacting', approvalRequestIds: [], userInputRequestIds: [], compactingSinceUnixMs: 1_754_000_000_000 },
  ]);

  await reconcileThreadLiveActivity('gpu');

  expect(read).toHaveBeenCalledTimes(1);
  expect(getActiveTurn('gpu-running')).toEqual({ turnId: 'round-7', turnIndex: 7, startedAt: 700 });
  expect(getThreadStatus('gpu-running')).toBe('running');
  expect(getThreadStatus('gpu-blocked')).toBe('awaiting-input');
  expect(isThreadCompacting('gpu-compacting')).toBe(true);
  expect(getActiveTurn('gpu-stale')).toBeNull();
  expect(getThreadStatus('gpu-stale')).toBe('idle');
  expect(getActiveTurn('laptop-running')?.turnId).toBe('laptop-round');
});

it('lets a push that landed during the read win over the snapshot', async () => {
  let answer!: (rows: unknown[]) => void;
  setBindingMock('ListThreadLiveActivity', () => new Promise<unknown[]>((resolve) => { answer = resolve; }));
  const pending = reconcileThreadLiveActivity('gpu');
  // provider:turn_started arrives while the snapshot is in flight; the
  // snapshot predates it and must not clear it.
  projectTurnStarted('gpu-stale', 'round-new', 3, 900);
  projectTurnStarted('gpu-running', 'round-8', 8, 800);
  answer([
    { threadId: 'gpu-running', activeTurn: { threadId: 'gpu-running', turnId: 'round-7', turnIndex: 7, startedAt: 700 }, approvalRequestIds: [], userInputRequestIds: [] },
  ]);
  await pending;

  expect(getActiveTurn('gpu-stale')?.turnId).toBe('round-new');
  expect(getActiveTurn('gpu-running')?.turnId).toBe('round-8');
});

it('reads nothing from a computer where the session lacks threads:read', async () => {
  vi.mocked(hasScope).mockReturnValue(false);
  const read = setBindingMock('ListThreadLiveActivity', async () => []);
  projectTurnStarted('gpu-stale', 'round-old', 2, 50);

  await reconcileThreadLiveActivity('gpu');

  expect(read).not.toHaveBeenCalled();
  expect(getBindingMock('ListThreadLiveActivity')).toBe(read);
  expect(getActiveTurn('gpu-stale')?.turnId).toBe('round-old');
});

it('rejects with the transport error and leaves the registries untouched', async () => {
  setBindingMock('ListThreadLiveActivity', async () => { throw new Error('offline'); });
  projectTurnStarted('gpu-stale', 'round-old', 2, 50);

  await expect(reconcileThreadLiveActivity('gpu')).rejects.toThrow('offline');

  expect(getActiveTurn('gpu-stale')?.turnId).toBe('round-old');
});

it('preserves requests and compaction events arriving during a sidebar snapshot', async () => {
  let answer!: (rows: unknown[]) => void;
  setBindingMock('ListThreadLiveActivity', () => new Promise(resolve => { answer = resolve; }));
  const pending = reconcileThreadLiveActivity('gpu');
  projectApprovalRequest('gpu-blocked', 'new-request');
  projectApprovalRequest('gpu-running', 'answered-request');
  projectApprovalResolution('gpu-running', 'answered-request');
  applyCompactingState({ threadId: 'gpu-running', active: true, sinceUnixMs: 100 });
  applyCompactingState({ threadId: 'gpu-running', active: false });
  applyCompactingState({ threadId: 'gpu-blocked', active: true, sinceUnixMs: 200 });
  answer([{ threadId: 'gpu-running', approvalRequestIds: ['answered-request'], compactingSinceUnixMs: 100 }]);
  await pending;
  expect(getThreadStatus('gpu-blocked')).toBe('pending-approval');
  expect(getThreadStatus('gpu-running')).toBe('idle');
  expect(isThreadCompacting('gpu-running')).toBe(false);
  expect(isThreadCompacting('gpu-blocked')).toBe(true);
});

it('discards a response for a thread moved to another computer during the read', async () => {
  let answer!: (rows: unknown[]) => void;
  setBindingMock('ListThreadLiveActivity', () => new Promise(resolve => { answer = resolve; }));
  const pending = reconcileThreadLiveActivity('gpu');
  noteThread('gpu-running', 'laptop', 2);
  answer([{ threadId: 'gpu-running', approvalRequestIds: ['old-owner'] }]);
  await pending;
  expect(getThreadStatus('gpu-running')).toBe('idle');
});

describe('reads of one computer', () => {
  const row = (turnId: string, turnIndex: number) => ({
    threadId: 'gpu-stale',
    activeTurn: { threadId: 'gpu-stale', turnId, turnIndex, startedAt: turnIndex * 100 },
    approvalRequestIds: [], userInputRequestIds: [],
  });
  type Call = { resolve: (rows: unknown[]) => void; reject: (err: Error) => void };
  function holdReads(): Call[] {
    const calls: Call[] = [];
    setBindingMock('ListThreadLiveActivity', () => new Promise((resolve, reject) => { calls.push({ resolve, reject }); }));
    return calls;
  }

  it('run one at a time, and every request made during a read shares one read after it', async () => {
    const calls = holdReads();
    const first = reconcileThreadLiveActivity('gpu');
    await vi.waitFor(() => expect(calls).toHaveLength(1));
    const second = reconcileThreadLiveActivity('gpu');
    const third = reconcileThreadLiveActivity('gpu');
    expect(third).toBe(second);
    await Promise.resolve();
    expect(calls).toHaveLength(1);

    calls[0].resolve([row('round-1', 1)]);
    await first;
    expect(getActiveTurn('gpu-stale')?.turnId).toBe('round-1');
    await vi.waitFor(() => expect(calls).toHaveLength(2));
    calls[1].resolve([row('round-2', 2)]);
    await second;
    expect(getActiveTurn('gpu-stale')?.turnId).toBe('round-2');
    expect(calls).toHaveLength(2);
  });

  it('apply each answer under sustained requests, never queueing more than one read', async () => {
    const calls = holdReads();
    let read = reconcileThreadLiveActivity('gpu');
    for (let round = 1; round <= 5; round++) {
      await vi.waitFor(() => expect(calls).toHaveLength(round));
      const next = reconcileThreadLiveActivity('gpu');
      reconcileThreadLiveActivity('gpu');
      calls[round - 1].resolve([row(`round-${round}`, round)]);
      await read;
      expect(getActiveTurn('gpu-stale')?.turnId).toBe(`round-${round}`);
      read = next;
    }
    await vi.waitFor(() => expect(calls).toHaveLength(6));
    calls[5].resolve([]);
    await read;
    expect(getActiveTurn('gpu-stale')).toBeNull();
  });

  it('keep the last answer when a read fails, and still run the read queued behind it', async () => {
    const calls = holdReads();
    projectTurnStarted('gpu-stale', 'round-old', 2, 50);
    const failing = reconcileThreadLiveActivity('gpu');
    await vi.waitFor(() => expect(calls).toHaveLength(1));
    const queued = reconcileThreadLiveActivity('gpu');
    calls[0].reject(new Error('client_overloaded'));
    await expect(failing).rejects.toThrow('client_overloaded');
    expect(getActiveTurn('gpu-stale')?.turnId).toBe('round-old');
    await vi.waitFor(() => expect(calls).toHaveLength(2));
    calls[1].resolve([row('round-9', 9)]);
    await queued;
    expect(getActiveTurn('gpu-stale')?.turnId).toBe('round-9');
  });
});

it('attributes a thread only the snapshot named, so a later snapshot settles it', async () => {
  setBindingMock('ListThreadLiveActivity', async () => [{
    threadId: 'gpu-unlisted',
    activeTurn: { threadId: 'gpu-unlisted', turnId: 'round-1', turnIndex: 1, startedAt: 10 },
    approvalRequestIds: [], userInputRequestIds: [],
  }]);
  await reconcileThreadLiveActivity('gpu');
  expect(threadBackend('gpu-unlisted')).toBe('gpu');
  expect(getThreadStatus('gpu-unlisted')).toBe('running');

  setBindingMock('ListThreadLiveActivity', async () => []);
  await reconcileThreadLiveActivity('gpu');
  expect(getThreadStatus('gpu-unlisted')).toBe('idle');
});

describe('sends pending', () => {
  const hello: TransportHello = {
    protocolVersion: 1, capabilities: ['sends-pending.v1'], backendId: 'gpu', backendName: 'gpu',
    serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0,
  };
  beforeEach(() => __setTransportHelloForTest(hello, 'gpu'));
  afterEach(() => __setTransportHelloForTest(null, 'gpu'));

  it('marks a named thread running and clears an unnamed one', async () => {
    // A clear this client missed while it was away.
    push('gpu-stale', true);
    setBindingMock('ListThreadLiveActivity', async () => [
      { threadId: 'gpu-running', approvalRequestIds: [], userInputRequestIds: [], sendsPending: true },
    ]);
    await reconcileThreadLiveActivity('gpu');
    expect(getThreadStatus('gpu-running')).toBe('running');
    expect(getThreadStatus('gpu-stale')).toBe('idle');
  });

  it('lets a push that landed during the read win over the snapshot', async () => {
    let answer!: (rows: unknown[]) => void;
    setBindingMock('ListThreadLiveActivity', () => new Promise(resolve => { answer = resolve; }));
    const pending = reconcileThreadLiveActivity('gpu');
    push('gpu-running', false);
    push('gpu-stale', true);
    answer([{ threadId: 'gpu-running', approvalRequestIds: [], userInputRequestIds: [], sendsPending: true }]);
    await pending;
    expect(getThreadStatus('gpu-running')).toBe('idle');
    expect(getThreadStatus('gpu-stale')).toBe('running');
  });

});

it("drops a detached computer's read in flight, whose threads it no longer owns, and the read queued behind it", async () => {
  const fakeClient = {
    callByID: async () => null, callByName: async () => null, subscribe: () => () => undefined,
    installStepUpProver: () => undefined, setLease: () => undefined, setWatchedThreads: () => undefined,
    getStatus: () => ({ status: 'connected', nextAttemptAt: null }), onStatusChange: () => () => undefined,
    getHello: () => null, onHelloChange: () => () => undefined, onReplay: () => () => undefined, close: () => undefined,
  };
  __attachBackendForTest({
    id: 'laptop', backendId: '99999999-8888-4777-8666-555555555555', name: 'Laptop',
    wsUrl: 'ws://localhost:3000/ws/backend/laptop', bootstrapUrl: '/bootstrap/laptop.json',
  }, fakeClient as never);
  try {
    let answer!: (rows: unknown[]) => void;
    let calls = 0;
    setBindingMock('ListThreadLiveActivity', () => new Promise<unknown[]>((resolve) => { calls++; answer = resolve; }));
    const read = reconcileThreadLiveActivity('laptop');
    await vi.waitFor(() => expect(answer).toBeDefined());
    const queued = reconcileThreadLiveActivity('laptop');

    detachBackend('laptop');
    answer([{
      threadId: 'laptop-running',
      activeTurn: { threadId: 'laptop-running', turnId: 'turn-1', turnIndex: 0, startedAt: 1 },
      approvalRequestIds: [],
      userInputRequestIds: [],
    }]);
    await read;
    await queued;

    expect(calls).toBe(1);
    expect(getActiveTurn('laptop-running')).toBeNull();
    expect(getThreadStatus('laptop-running')).toBe('idle');
  } finally {
    __resetBackendsForTest();
  }
});
