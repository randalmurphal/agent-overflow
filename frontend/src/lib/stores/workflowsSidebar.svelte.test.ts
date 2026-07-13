import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import type { WorkItem } from '../types/workflow';
import {
  applyWorkflowSidebarItemState,
  applyWorkflowSidebarPhaseState,
  applyWorkflowSidebarQueueState,
  getGlobalWorkflowAttentionCount,
  getProjectWorkflowAttentionCount,
  getProjectWorkflowDefinitions,
  getProjectWorkflowRuns,
  getWorkflowSidebarPhaseProgress,
  initializeWorkflowsSidebar,
  isWorkflowSidebarInitialized,
  refreshWorkflowsSidebar,
  resetWorkflowsSidebarForTest,
} from './workflowsSidebar.svelte';
import { addProjectLocal, resetProjectsForTest } from './projects.svelte';

function run(id: string, projectId: string, state: string, disposition = ''): WorkItem {
  return {
    id, projectId, workflowId: 'wf', workflowScope: 'shared', goal: id,
    state, reason: state === 'needs-human' ? 'gate' : '', sortPosition: 1,
    stepMode: false, source: 'manual', createdAt: 1, startedAt: 1, disposition,
  } as WorkItem;
}

function catalog() {
  return {
    baseBranch: 'main', predictedQueuePosition: 1,
    workflows: [{
      id: 'wf', name: 'Workflow', scope: 'shared', phaseCount: 2,
      phases: [{ id: 'plan' }, { id: 'build' }], inputs: [], defaultStepMode: false,
      valid: true, allBindingsAvailable: true,
    }],
  };
}

describe('workflows sidebar store', () => {
  beforeEach(() => {
    resetProjectsForTest();
    resetWorkflowsSidebarForTest();
    setBindingMock('WorkflowListUnresolvedItems', async () => [
      run('attention', 'p1', 'needs-human'),
      run('failed', 'p1', 'failed'),
      run('resolved', 'p2', 'done', '{"action":"merged","policy":"manual","at":1}'),
    ]);
    setBindingMock('WorkflowListDefinitions', async () => catalog());
  });

  afterEach(() => {
    resetProjectsForTest();
    resetBindingMocks();
    vi.restoreAllMocks();
  });

  it('initializes once from the app-wide item fetch and project catalogs', async () => {
    await Promise.all([initializeWorkflowsSidebar(), initializeWorkflowsSidebar()]);
    expect(getProjectWorkflowRuns('p1').map((item) => item.id)).toEqual(['attention', 'failed']);
    expect(getProjectWorkflowRuns('p2')).toEqual([]);
    expect(getProjectWorkflowDefinitions('p1')).toHaveLength(1);
    expect(getProjectWorkflowAttentionCount('p1')).toBe(2);
    expect(getGlobalWorkflowAttentionCount()).toBe(2);
  });

  it('loads definitions for a known project with no runs', async () => {
    addProjectLocal({
      id: 'runless', name: 'Runless', path: '/tmp/runless', sortPosition: 0,
      createdAt: 1, updatedAt: 1, archived: false,
    });
    await initializeWorkflowsSidebar();
    expect(getProjectWorkflowRuns('runless')).toEqual([]);
    expect(getProjectWorkflowDefinitions('runless')).toHaveLength(1);
  });

  it('resolves phase progress only after a phase event this session', async () => {
    await initializeWorkflowsSidebar();
    const item = getProjectWorkflowRuns('p1')[0]!;
    expect(getWorkflowSidebarPhaseProgress(item)).toBeNull();
    applyWorkflowSidebarPhaseState({ itemId: item.id, phaseId: 'build', attempt: 1, status: 'running' });
    expect(getWorkflowSidebarPhaseProgress(item)).toEqual({ current: 2, total: 2, phaseId: 'build' });
  });

  it('patches event state immediately', async () => {
    await initializeWorkflowsSidebar();
    applyWorkflowSidebarItemState({ itemId: 'attention', projectId: 'p1', from: 'needs-human', to: 'running' });
    expect(getProjectWorkflowAttentionCount('p1')).toBe(1);
    expect(getProjectWorkflowRuns('p1').find((item) => item.id === 'attention')?.state).toBe('running');
  });

  it('refreshes summary fields omitted by a known item-state transition', async () => {
    setBindingMock('WorkflowListUnresolvedItems', async () => [run('known', 'p1', 'queued')]);
    await initializeWorkflowsSidebar();
    const definitions = getBindingMock('WorkflowListDefinitions');
    setBindingMock('WorkflowListUnresolvedItems', async () => [{
      ...run('known', 'p1', 'running'),
      startedAt: 42,
      worktreePath: '/tmp/worktree',
      baseBranch: 'main',
    }]);

    applyWorkflowSidebarItemState({ itemId: 'known', projectId: 'p1', from: 'queued', to: 'running' });
    expect(getProjectWorkflowRuns('p1')[0]?.state).toBe('running');
    await vi.waitFor(() => expect(getProjectWorkflowRuns('p1')[0]).toMatchObject({
      startedAt: 42,
      worktreePath: '/tmp/worktree',
      baseBranch: 'main',
    }));
    expect(definitions).toHaveBeenCalledTimes(1);
  });

  it('does not let an in-flight refresh overwrite a newer item event', async () => {
    await initializeWorkflowsSidebar();
    let release!: (items: WorkItem[]) => void;
    setBindingMock('WorkflowListUnresolvedItems', () => new Promise<WorkItem[]>((resolve) => { release = resolve; }));

    applyWorkflowSidebarQueueState();
    await vi.waitFor(() => expect(release).toBeTypeOf('function'));
    applyWorkflowSidebarItemState({ itemId: 'attention', projectId: 'p1', from: 'needs-human', to: 'running' });
    release([
      run('attention', 'p1', 'needs-human'),
      run('failed', 'p1', 'failed'),
    ]);

    await vi.waitFor(() => {
      expect(getProjectWorkflowRuns('p1').find((item) => item.id === 'attention')?.state).toBe('running');
    });
  });

  it('recovers from a transient boot fetch failure when refresh is requested', async () => {
    setBindingMock('WorkflowListUnresolvedItems', async () => { throw new Error('transport unavailable'); });
    await initializeWorkflowsSidebar();
    expect(isWorkflowSidebarInitialized()).toBe(false);

    setBindingMock('WorkflowListUnresolvedItems', async () => [run('recovered', 'p1', 'running')]);
    refreshWorkflowsSidebar();
    await vi.waitFor(() => expect(isWorkflowSidebarInitialized()).toBe(true));
    expect(getProjectWorkflowRuns('p1').map((item) => item.id)).toEqual(['recovered']);
  });

  it('replays a queue refresh requested during the initial fetch', async () => {
    let releaseInitial!: (items: WorkItem[]) => void;
    let calls = 0;
    setBindingMock('WorkflowListUnresolvedItems', () => {
      calls += 1;
      if (calls === 1) return new Promise<WorkItem[]>((resolve) => { releaseInitial = resolve; });
      return Promise.resolve([
        { ...run('b', 'p1', 'queued'), sortPosition: 0 },
        { ...run('a', 'p1', 'queued'), sortPosition: 1 },
      ]);
    });

    const initializing = initializeWorkflowsSidebar();
    await vi.waitFor(() => expect(releaseInitial).toBeTypeOf('function'));
    applyWorkflowSidebarQueueState();
    releaseInitial([
      { ...run('a', 'p1', 'queued'), sortPosition: 0 },
      { ...run('b', 'p1', 'queued'), sortPosition: 1 },
    ]);
    await initializing;

    await vi.waitFor(() => {
      expect(calls).toBe(2);
      expect(getProjectWorkflowRuns('p1').map((item) => item.id)).toEqual(['b', 'a']);
    });
  });
});
