import { beforeEach, describe, expect, it, vi } from 'vitest';
import { getToasts, removeToast } from './toast.svelte';
import {
  activateWorkflowsPane,
  getWorkflowDefinitions,
  getWorkflowQueueState,
  loadWorkflowOverview,
  resetWorkflowsPane,
} from './workflowsPane.svelte';
import {
  getProjectWorkflowAttentionCount,
  initializeWorkflowsSidebar,
  resetWorkflowsSidebarForTest,
} from './workflowsSidebar.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { getBindingMock } from '../../test/mocks/bindings-app';
import {
  applyWorkflowDefinitionsChangedEvent,
  applyWorkflowErrorEvent,
  applyWorkflowItemStateEvent,
  applyWorkflowQueueStateEvent,
  resetWorkflowEventStateForTest,
} from './eventsWorkflow';
import { addProjectLocal, resetProjectsForTest } from './projects.svelte';

describe('workflow event fan-out', () => {
  beforeEach(() => {
    resetWorkflowsPane();
    resetWorkflowsSidebarForTest();
    resetBindingMocks();
    resetProjectsForTest();
    setBindingMock('WorkflowListUnresolvedItems', async () => [{
      id: 'run', projectId: 'p', workflowId: 'wf', state: 'running', createdAt: 1, sortPosition: 1,
    }]);
    setBindingMock('WorkflowListDefinitions', async () => ({
      baseBranch: 'main', predictedQueuePosition: 1, workflows: [],
    }));
    setBindingMock('WorkflowListItems', async () => []);
    setBindingMock('WorkflowListItemCosts', async () => ({}));
    resetWorkflowEventStateForTest();
    for (const toast of getToasts()) removeToast(toast.id);
  });

  it('fans queue state into the pane and refreshes sidebar summaries', async () => {
    await initializeWorkflowsSidebar();
    applyWorkflowQueueStateEvent({ active: false, globalConcurrency: 3, runningCount: 2, slotCapacity: 3, startsRemaining: 2 });
    expect(getWorkflowQueueState()).toEqual({
      active: false, globalConcurrency: 3, runningCount: 2, slotCapacity: 3,
      startsRemaining: 2, projects: [],
    });
    await vi.waitFor(() => expect(getBindingMock('WorkflowListUnresolvedItems')).toHaveBeenCalledTimes(2));
  });

  it('keeps old queue events compatible by deriving additive slot fields', () => {
    applyWorkflowQueueStateEvent({ active: true, globalConcurrency: 4 });

    expect(getWorkflowQueueState()).toMatchObject({ runningCount: 0, slotCapacity: 4, projects: [] });
  });

  it('refreshes the active pane definition catalog after a filesystem event', async () => {
    addProjectLocal({
      id: 'p', path: '/tmp/p', name: 'Project', sortPosition: 0,
      createdAt: 1, updatedAt: 1, archived: false,
    });
    let name = 'Before';
    setBindingMock('WorkflowListDefinitions', async () => ({
      baseBranch: 'main', predictedQueuePosition: 1,
      workflows: [{
        id: 'wf', name, scope: 'shared', phaseCount: 0, humanGateCount: 0,
        phases: [], inputs: [], defaultStepMode: false, valid: true, allBindingsAvailable: true,
      }],
    }));
    activateWorkflowsPane();
    await loadWorkflowOverview();
    expect(getWorkflowDefinitions()[0]?.definition.name).toBe('Before');
    name = 'After';
    applyWorkflowDefinitionsChangedEvent();
    await vi.waitFor(() => expect(getWorkflowDefinitions()[0]?.definition.name).toBe('After'));
  });

  it('fans item state into the always-on sidebar store', async () => {
    await initializeWorkflowsSidebar();
    applyWorkflowItemStateEvent({ itemId: 'run', projectId: 'p', from: 'running', to: 'needs-human', reason: 'gate' });
    expect(getProjectWorkflowAttentionCount('p')).toBe(1);
  });

  it('deduplicates user-facing errors by item and message', () => {
    applyWorkflowErrorEvent({ itemId: 'a', error: 'Provider stopped' });
    applyWorkflowErrorEvent({ itemId: 'a', error: 'Provider stopped' });
    applyWorkflowErrorEvent({ itemId: 'b', error: 'Provider stopped' });
    expect(getToasts().map((toast) => toast.message)).toEqual(['Provider stopped', 'Provider stopped']);
  });
});
