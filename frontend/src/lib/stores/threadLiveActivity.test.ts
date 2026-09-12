import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { reconcileThreadLiveActivity } from './threadLiveActivity';
import { getBindingMock, setBindingMock } from '../../test/mocks/bindings-app';
import { __resetEntityIndexForTest, noteThread } from '../transport/entityIndex';
import {
  getActiveTurn,
  getThreadStatus,
  projectApprovalRequest,
  projectApprovalResolution,
  projectTurnStarted,
  resetForTest,
} from './threadStatuses.svelte';
import { applyCompactingState, isThreadCompacting } from './compactingState.svelte';
import { hasScope } from '../transport/scopes';

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
