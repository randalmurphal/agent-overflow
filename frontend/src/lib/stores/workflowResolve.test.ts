import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { WorkItem } from '../types/workflow';
import type { WorkflowActionBindings } from './workflowActions';
import { WORKFLOW_AUTO_ADVANCE_MS, cancelWorkflowAutoAdvance, resolveWorkflowRun } from './workflowResolve';
import {
  getWorkflowReceipt,
  hydrateWorkflowAttention,
  resetWorkflowRunsForTest,
} from './workflowRuns.svelte';
import {
  getWorkflowArmedAction,
  getWorkflowsOverlayRunId,
  getWorkflowsOverlayTop,
  openWorkflowsOverlay,
  pushWorkflowRunDetail,
  resetWorkflowsOverlayForTest,
  setWorkflowArmedAction,
} from './workflowsOverlay.svelte';
import { getToasts, removeToast } from './toast.svelte';
import { resetAppStorageForTest } from './appStorage';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';

function item(id: string, endedAt: number): WorkItem {
  return { id, projectId: 'p', state: 'needs-human', reason: 'gate', endedAt, createdAt: endedAt } as WorkItem;
}

function bindings(over: Partial<WorkflowActionBindings> = {}): WorkflowActionBindings {
  return {
    answerQuestion: vi.fn(async () => undefined),
    cancelItem: vi.fn(async () => undefined),
    completeTakeover: vi.fn(async () => undefined),
    createPR: vi.fn(async () => ({ prRef: '#1' })),
    discardItem: vi.fn(async () => ({ discarded: { removedWorktrees: [] } })),
    dropUnit: vi.fn(async () => undefined),
    mergeItem: vi.fn(async () => ({ base: 'main', mode: 'ff', sha: 'abc' })),
    pauseItem: vi.fn(async () => undefined),
    requestSoftStop: vi.fn(async () => undefined),
    resolveGate: vi.fn(async () => undefined),
    resumeItem: vi.fn(async () => undefined),
    rerunItem: vi.fn(async () => undefined),
    retryUnit: vi.fn(async () => undefined),
    retryFailedUnits: vi.fn(async () => undefined),
    ...over,
  };
}

function clearToasts(): void {
  for (const toast of [...getToasts()]) removeToast(toast.id);
}

describe('resolveWorkflowRun', () => {
  const parked = [item('run-1', 10), item('run-2', 20)];

  beforeEach(async () => {
    vi.useFakeTimers();
    resetBindingMocks();
    resetAppStorageForTest();
    resetWorkflowsOverlayForTest();
    resetWorkflowRunsForTest();
    clearToasts();
    setBindingMock('WorkflowListUnresolvedItems', async () => parked);
    await hydrateWorkflowAttention();
  });

  afterEach(() => {
    cancelWorkflowAutoAdvance();
    vi.useRealTimers();
    resetBindingMocks();
    clearToasts();
  });

  it('records the session receipt and toasts it', async () => {
    expect(await resolveWorkflowRun(parked[0], { kind: 'approve' }, 1.5, bindings())).toBe(true);
    expect(getWorkflowReceipt('run-1')).toMatchObject({ kind: 'approved', costUsd: 1.5 });
    expect(getToasts().at(-1)).toMatchObject({ type: 'success' });
  });

  // The whole-attempt repair is not a second path: it goes through the same
  // dispatch → receipt → toast → sweep steps every other action does.
  it('runs the whole-attempt unit repair through the one resolution path', async () => {
    const deps = bindings();
    expect(await resolveWorkflowRun(parked[0], { kind: 'retry-failed-units', note: '' }, 4, deps)).toBe(true);
    expect(deps.retryFailedUnits).toHaveBeenCalledWith('run-1', '');
    expect(getWorkflowReceipt('run-1')).toMatchObject({ kind: 'restarted', costUsd: 4 });
    expect(getToasts().at(-1)).toMatchObject({ type: 'success' });
  });

  it('disarms whatever confirm was armed', async () => {
    setWorkflowArmedAction('discard:run-1');
    await resolveWorkflowRun(parked[0], { kind: 'approve' }, 0, bindings());
    expect(getWorkflowArmedAction()).toBeNull();
  });

  it('records no receipt for pause and stop — the park is the receipt', async () => {
    expect(await resolveWorkflowRun(parked[0], { kind: 'pause' }, 0, bindings())).toBe(true);
    expect(getWorkflowReceipt('run-1')).toBeUndefined();
    expect(getToasts().at(-1)).toMatchObject({ type: 'info', message: expect.stringContaining('Paused') });

    clearToasts();
    expect(await resolveWorkflowRun(parked[0], { kind: 'cancel' }, 0, bindings())).toBe(true);
    expect(getToasts().at(-1)).toMatchObject({ type: 'info', message: expect.stringContaining('Stopping') });
  });

  it('auto-advances the sweep after the receipt has had time to read', async () => {
    openWorkflowsOverlay();
    pushWorkflowRunDetail('run-1', { sweep: true, sweepIndex: 0 });
    await resolveWorkflowRun(parked[0], { kind: 'approve' }, 0, bindings());
    expect(getWorkflowsOverlayRunId()).toBe('run-1');
    await vi.advanceTimersByTimeAsync(WORKFLOW_AUTO_ADVANCE_MS);
    expect(getWorkflowsOverlayRunId()).toBe('run-2');
  });

  it('lands on all-clear once the last parked run is resolved', async () => {
    openWorkflowsOverlay();
    pushWorkflowRunDetail('run-1', { sweep: true, sweepIndex: 0 });
    await resolveWorkflowRun(parked[0], { kind: 'approve' }, 0, bindings());
    await resolveWorkflowRun(parked[1], { kind: 'approve' }, 0, bindings());
    await vi.advanceTimersByTimeAsync(WORKFLOW_AUTO_ADVANCE_MS);
    expect(getWorkflowsOverlayTop()).toEqual({ level: 'all-clear' });
  });

  it('stays put when the human is not sweeping', async () => {
    openWorkflowsOverlay();
    pushWorkflowRunDetail('run-1', { sweep: false, sweepIndex: -1 });
    await resolveWorkflowRun(parked[0], { kind: 'approve' }, 0, bindings());
    await vi.advanceTimersByTimeAsync(WORKFLOW_AUTO_ADVANCE_MS * 4);
    expect(getWorkflowsOverlayRunId()).toBe('run-1');
  });

  it('drops a pending advance when the overlay session ends', async () => {
    openWorkflowsOverlay();
    pushWorkflowRunDetail('run-1', { sweep: true, sweepIndex: 0 });
    await resolveWorkflowRun(parked[0], { kind: 'approve' }, 0, bindings());
    cancelWorkflowAutoAdvance();
    await vi.advanceTimersByTimeAsync(WORKFLOW_AUTO_ADVANCE_MS * 4);
    expect(getWorkflowsOverlayRunId()).toBe('run-1');
  });

  it('reports a failure and leaves the run where it was', async () => {
    openWorkflowsOverlay();
    pushWorkflowRunDetail('run-1', { sweep: true, sweepIndex: 0 });
    const failing = bindings({ resolveGate: vi.fn(async () => { throw new Error('engine is down'); }) });
    expect(await resolveWorkflowRun(parked[0], { kind: 'approve' }, 0, failing)).toBe(false);
    expect(getWorkflowReceipt('run-1')).toBeUndefined();
    expect(getToasts().at(-1)).toMatchObject({ type: 'error' });
    await vi.advanceTimersByTimeAsync(WORKFLOW_AUTO_ADVANCE_MS * 4);
    expect(getWorkflowsOverlayRunId()).toBe('run-1');
  });
});
