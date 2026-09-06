import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { deferred } from '../../test/helpers/providerAccounts';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { detachBackend, takePinnedBackend } from '../transport/backends';
import { __resetEntityIndexForTest, noteWorkflowItem, workflowItemBackend } from '../transport/entityIndex';
import type { WorkItem, WorkflowItemDetail } from '../types/workflow';
import {
  applyWorkflowEngineState, applyWorkflowItemState, applyWorkflowSoftStop, getWorkflowDetail, getWorkflowRuns, hydrateWorkflowAttention,
  isWorkflowEnginePaused, loadWorkflowDetail, resetWorkflowRunsForTest,
  resyncWorkflowEngineState, retainWorkflowDetails,
} from './workflowRuns.svelte';

function row(id: string): WorkItem { return { id, projectId: id, state: 'running' } as WorkItem; }
function detail(id: string, goal: string): WorkflowItemDetail { return { item: { id, goal } } as WorkflowItemDetail; }

beforeEach(() => { resetStagedBackends(); resetWorkflowRunsForTest(); __resetEntityIndexForTest(); resetBindingMocks(); });
afterEach(() => { resetWorkflowRunsForTest(); resetStagedBackends(); __resetEntityIndexForTest(); resetBindingMocks(); vi.useRealTimers(); });

it('combines computer catalogs, retains an offline computer, and drops its rows on removal', async () => {
  stageBackend({ id: 'gpu' });
  let offline = false;
  setBindingMock('WorkflowListUnresolvedItems', async () => {
    const backend = takePinnedBackend();
    if (backend === 'gpu' && offline) throw new Error('offline');
    return [row(backend || 'mac')];
  });
  await hydrateWorkflowAttention();
  expect(getWorkflowRuns().map((run) => run.id)).toEqual(['mac', 'gpu']);
  expect(workflowItemBackend('gpu')).toBe('gpu');
  offline = true;
  await hydrateWorkflowAttention();
  expect(getWorkflowRuns().map((run) => run.id)).toEqual(['mac', 'gpu']);
  detachBackend('gpu');
  expect(getWorkflowRuns().map((run) => run.id)).toEqual(['mac']);
});

it('applies a slow first catalog without waiting for an unrelated future event', async () => {
  vi.useFakeTimers();
  stageBackend({ id: 'gpu' });
  const slow = deferred<WorkItem[]>();
  setBindingMock('WorkflowListUnresolvedItems', () => takePinnedBackend() === 'gpu' ? slow.promise : Promise.resolve([row('mac')]));
  const load = hydrateWorkflowAttention();
  await vi.advanceTimersByTimeAsync(2500);
  await load;
  expect(getWorkflowRuns().map((run) => run.id)).toEqual(['mac']);
  slow.resolve([row('gpu')]);
  await vi.advanceTimersByTimeAsync(0);
  expect(getWorkflowRuns().map((run) => run.id).sort()).toEqual(['gpu', 'mac']);
});

it('scopes engine state and discards a snapshot older than a live pause event', async () => {
  stageBackend({ id: 'gpu' });
  applyWorkflowEngineState({ paused: true }, '');
  expect(isWorkflowEnginePaused()).toBe(false);
  const slow = deferred<{ paused: boolean }>();
  setBindingMock('WorkflowGetEngineState', () => { expect(takePinnedBackend()).toBe('gpu'); return slow.promise; });
  const read = resyncWorkflowEngineState('gpu');
  applyWorkflowEngineState({ paused: true }, 'gpu');
  slow.resolve({ paused: false });
  await read;
  expect(isWorkflowEnginePaused()).toBe(true);
});

it('live run transitions and stop requests supersede older list snapshots', async () => {
  vi.useFakeTimers();
  setBindingMock('WorkflowListUnresolvedItems', async () => [row('run')]);
  await hydrateWorkflowAttention();
  const slow = deferred<WorkItem[]>();
  setBindingMock('WorkflowListUnresolvedItems', () => slow.promise);
  const load = hydrateWorkflowAttention();
  applyWorkflowItemState({ itemId: 'run', projectId: 'run', from: 'running', to: 'needs-human', reason: 'disposition' });
  applyWorkflowSoftStop({ itemId: 'run', armed: true });
  slow.resolve([row('run')]);
  await load;
  expect(getWorkflowRuns()[0].state).toBe('needs-human');
  expect(getWorkflowRuns()[0].softStop).toBe(true);
});

it('a late forced detail cannot replace a newer read, refill a closed pane, or revive a removed computer', async () => {
  stageBackend({ id: 'gpu' });
  noteWorkflowItem('run', 'gpu');
  const first = deferred<WorkflowItemDetail>();
  const second = deferred<WorkflowItemDetail>();
  let reads = 0;
  setBindingMock('WorkflowGetItem', () => { expect(takePinnedBackend()).toBe('gpu'); return ++reads === 1 ? first.promise : second.promise; });
  const old = loadWorkflowDetail('run');
  const fresh = loadWorkflowDetail('run', true);
  second.resolve(detail('run', 'new'));
  await fresh;
  first.resolve(detail('run', 'old'));
  await old;
  expect(getWorkflowDetail('run')?.item.goal).toBe('new');
  const closing = deferred<WorkflowItemDetail>();
  setBindingMock('WorkflowGetItem', () => closing.promise);
  const closedRead = loadWorkflowDetail('run', true);
  retainWorkflowDetails(null);
  closing.resolve(detail('run', 'closed'));
  await closedRead;
  expect(getWorkflowDetail('run')).toBeUndefined();
  const removing = deferred<WorkflowItemDetail>();
  setBindingMock('WorkflowGetItem', () => removing.promise);
  const removedRead = loadWorkflowDetail('run');
  detachBackend('gpu');
  removing.resolve(detail('run', 'removed'));
  await removedRead;
  expect(getWorkflowDetail('run')).toBeUndefined();
});
