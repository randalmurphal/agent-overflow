import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { WorkItem, WorkflowItemDetail } from '../types/workflow';
import {
  applyWorkflowEngineState,
  applyWorkflowItemState,
  applyWorkflowPhaseState,
  clearWorkflowReceipts,
  getWorkflowAttentionCount,
  getWorkflowAutomations,
  getWorkflowCatalog,
  getWorkflowCosts,
  getWorkflowDetail,
  getWorkflowLivePhase,
  getWorkflowReceipt,
  getWorkflowRun,
  getWorkflowRuns,
  getWorkflowLoadError,
  hydrateWorkflowAttention,
  isWorkflowEnginePaused,
  isWorkflowOverlayLoaded,
  loadWorkflowDetail,
  loadWorkflowsOverlayData,
  recordWorkflowReceipt,
  resetWorkflowRunsForTest,
  retainWorkflowDetails,
} from './workflowRuns.svelte';
import { getToasts, removeToast } from './toast.svelte';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';

function item(id: string, over: Partial<WorkItem> = {}): WorkItem {
  return { id, projectId: 'p1', state: 'needs-human', reason: 'gate', createdAt: 1, ...over } as WorkItem;
}

function detail(id: string, children: string[] = []): WorkflowItemDetail {
  return {
    item: { id },
    checkPhaseIds: [],
    phases: [],
    units: [],
    children: children.map((childId) => ({ itemId: childId })),
    outputs: {},
    artifacts: [],
    usage: { costUsd: 0 },
  } as unknown as WorkflowItemDetail;
}

function overlayBindings(items: WorkItem[]): void {
  setBindingMock('WorkflowListItems', async () => items);
  setBindingMock('WorkflowGetEngineState', async () => ({ paused: true }));
  setBindingMock('WorkflowListDefinitions', async (projectId: string) => ({
    baseBranch: 'main',
    workflows: [{ id: `${projectId}-flow`, scope: 'project' }],
  }));
  setBindingMock('WorkflowListAutomations', async () => [{ id: 'auto-1', workflowId: 'p1-flow' }]);
  setBindingMock('WorkflowListItemCosts', async (projectId: string) =>
    projectId === 'p1' ? { 'run-1': 2.5, 'run-bad': 'nope' } : {});
}

function clearToasts(): void {
  for (const toast of [...getToasts()]) removeToast(toast.id);
}

describe('workflowRuns store', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWorkflowRunsForTest();
    clearToasts();
    vi.spyOn(console, 'warn').mockImplementation(() => {});
  });

  afterEach(() => {
    vi.restoreAllMocks();
    resetBindingMocks();
    resetWorkflowRunsForTest();
    clearToasts();
  });

  describe('attention hydration (§6)', () => {
    it('fills the badge from the unresolved listing', async () => {
      setBindingMock('WorkflowListUnresolvedItems', async () => [
        item('run-1'),
        item('run-2', { state: 'done', reason: '' }),
      ]);
      await hydrateWorkflowAttention();
      expect(getWorkflowRuns()).toHaveLength(2);
      // done-awaiting-disposition is neutral, never amber (R1).
      expect(getWorkflowAttentionCount()).toBe(1);
    });

    it('stays quiet when the backend has no workflow engine — no toast on every launch', async () => {
      setBindingMock('WorkflowListUnresolvedItems', async () => { throw new Error('unavailable'); });
      await hydrateWorkflowAttention();
      expect(getWorkflowRuns()).toEqual([]);
      expect(getToasts()).toHaveLength(0);
      expect(console.warn).toHaveBeenCalled();
    });
  });

  describe('overlay hydration', () => {
    it('loads runs, engine state, and per-project catalogs for every project carrying a run', async () => {
      overlayBindings([item('run-1'), item('run-9', { projectId: 'p2' })]);
      await loadWorkflowsOverlayData(['p1']);

      expect(isWorkflowOverlayLoaded()).toBe(true);
      expect(isWorkflowEnginePaused()).toBe(true);
      expect(getWorkflowCatalog('p1')?.workflows[0].id).toBe('p1-flow');
      // p2 was never in the sidebar list; its run is what pulls its catalog in.
      expect(getWorkflowCatalog('p2')?.workflows[0].id).toBe('p2-flow');
      expect(getWorkflowAutomations('p1')).toHaveLength(1);
      // A non-numeric cost is dropped rather than rendered as NaN.
      expect(getWorkflowCosts()).toEqual({ 'run-1': 2.5 });
    });

    it('surfaces a listing failure as user-facing state, not a silent empty overlay', async () => {
      overlayBindings([]);
      setBindingMock('WorkflowListItems', async () => { throw new Error('engine is down'); });
      await loadWorkflowsOverlayData(['p1']);
      expect(isWorkflowOverlayLoaded()).toBe(false);
      expect(getWorkflowLoadError()).toBe('Engine is down.');
      expect(getToasts().at(-1)).toMatchObject({ type: 'error' });
    });

    it('coalesces concurrent opens into one listing', async () => {
      overlayBindings([item('run-1')]);
      await Promise.all([loadWorkflowsOverlayData(['p1']), loadWorkflowsOverlayData(['p1'])]);
      expect(getBindingMock('WorkflowListItems')).toHaveBeenCalledTimes(1);
    });
  });

  describe('run detail', () => {
    beforeEach(() => {
      setBindingMock('WorkflowGetItem', async (itemId: string) => detail(itemId));
    });

    it('is single-flight per run and cached afterwards', async () => {
      await Promise.all([loadWorkflowDetail('run-1'), loadWorkflowDetail('run-1')]);
      expect(getBindingMock('WorkflowGetItem')).toHaveBeenCalledTimes(1);
      await loadWorkflowDetail('run-1');
      expect(getBindingMock('WorkflowGetItem')).toHaveBeenCalledTimes(1);
      await loadWorkflowDetail('run-1', true);
      expect(getBindingMock('WorkflowGetItem')).toHaveBeenCalledTimes(2);
      expect(getWorkflowDetail('run-1')).toBeDefined();
    });

    it('reports a load failure instead of leaving the surface on "Loading run…" forever', async () => {
      setBindingMock('WorkflowGetItem', async () => { throw new Error('no such run'); });
      expect(await loadWorkflowDetail('run-1')).toBeNull();
      expect(getToasts().at(-1)).toMatchObject({ type: 'error' });
    });
  });

  describe('retainWorkflowDetails — the memory bound (principle 4)', () => {
    beforeEach(() => {
      setBindingMock('WorkflowGetItem', async (itemId: string) =>
        itemId === 'root' ? detail('root', ['child']) : detail(itemId));
    });

    it('keeps the focused root alone — a called run is the run map\'s job, not this cache\'s', async () => {
      await loadWorkflowDetail('root');
      await loadWorkflowDetail('child');
      await loadWorkflowDetail('stranger');

      retainWorkflowDetails('root');
      expect(getWorkflowDetail('root')).toBeDefined();
      expect(getWorkflowDetail('child')).toBeUndefined();
      expect(getWorkflowDetail('stranger')).toBeUndefined();
    });

    it('drops everything when the focused root has no detail of its own', async () => {
      await loadWorkflowDetail('stranger');
      retainWorkflowDetails('root');
      expect(getWorkflowDetail('stranger')).toBeUndefined();
    });

    it('drops everything when the detail level is left', async () => {
      await loadWorkflowDetail('root');
      retainWorkflowDetails(null);
      expect(getWorkflowDetail('root')).toBeUndefined();
    });

    it('writes nothing when there is nothing to drop — including before the root has landed', async () => {
      // Regression: the guard used to compare `keep.size` against the cache
      // size, so a root whose detail had not loaded yet looked like a cache
      // that needed rewriting on EVERY call. Inside the overlay's $effect —
      // which reads the same cache — that is an infinite effect loop, and the
      // run detail never leaves "Loading run…".
      const empty = getWorkflowDetail('root');
      retainWorkflowDetails('root');
      expect(getWorkflowDetail('root')).toBe(empty);

      await loadWorkflowDetail('root');
      const loaded = getWorkflowDetail('root');
      retainWorkflowDetails('root');
      retainWorkflowDetails('root');
      expect(getWorkflowDetail('root')).toBe(loaded);
    });
  });

  describe('event application', () => {
    it('patches a run the cache already knows', async () => {
      setBindingMock('WorkflowListUnresolvedItems', async () => [item('run-1')]);
      await hydrateWorkflowAttention();
      applyWorkflowItemState({ itemId: 'run-1', projectId: 'p1', from: 'needs-human', to: 'done' });
      expect(getWorkflowRun('run-1')?.state).toBe('done');
      expect(getWorkflowAttentionCount()).toBe(0);
    });

    it('refreshes a cached detail in place and forgets the live phase once the run rests', async () => {
      setBindingMock('WorkflowGetItem', async (itemId: string) => detail(itemId));
      await loadWorkflowDetail('run-1');
      applyWorkflowPhaseState({
        itemId: 'run-1', phaseId: 'plan', attempt: 1, status: 'running', unitId: '', occurredAt: 1_000,
      });
      expect(getWorkflowLivePhase('run-1')).toMatchObject({ phaseId: 'plan', status: 'running' });
      expect(getBindingMock('WorkflowGetItem')).toHaveBeenCalledTimes(2);

      applyWorkflowItemState({ itemId: 'run-1', projectId: 'p1', from: 'running', to: 'done' });
      expect(getWorkflowLivePhase('run-1')).toBeUndefined();
      expect(getBindingMock('WorkflowGetItem')).toHaveBeenCalledTimes(3);
    });

    it('ignores a malformed event rather than writing a phantom run', () => {
      applyWorkflowItemState({ itemId: '', projectId: 'p1', from: '', to: 'done' } as never);
      applyWorkflowPhaseState({ itemId: '' } as never);
      applyWorkflowEngineState({ paused: 'yes' } as never);
      expect(getWorkflowRuns()).toEqual([]);
      expect(isWorkflowEnginePaused()).toBe(false);
    });
  });

  it('holds session receipts until the overlay session ends', () => {
    recordWorkflowReceipt({ itemId: 'run-1', kind: 'approved', message: 'Approved', costUsd: 1 });
    expect(getWorkflowReceipt('run-1')).toMatchObject({ kind: 'approved' });
    clearWorkflowReceipts();
    expect(getWorkflowReceipt('run-1')).toBeUndefined();
  });
});
