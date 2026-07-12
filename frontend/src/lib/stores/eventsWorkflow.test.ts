import { beforeEach, describe, expect, it, vi } from 'vitest';
import { getToasts, removeToast } from './toast.svelte';
import { getWorkflowQueueState, resetWorkflowsPane } from './workflowsPane.svelte';
import {
  getProjectWorkflowAttentionCount,
  initializeWorkflowsSidebar,
  resetWorkflowsSidebarForTest,
} from './workflowsSidebar.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { getBindingMock } from '../../test/mocks/bindings-app';
import {
  applyWorkflowErrorEvent,
  applyWorkflowItemStateEvent,
  applyWorkflowQueueStateEvent,
  resetWorkflowEventStateForTest,
} from './eventsWorkflow';

describe('workflow event fan-out', () => {
  beforeEach(() => {
    resetWorkflowsPane();
    resetWorkflowsSidebarForTest();
    resetBindingMocks();
    setBindingMock('WorkflowListItems', async () => [{
      id: 'run', projectId: 'p', workflowId: 'wf', state: 'running', createdAt: 1, sortPosition: 1,
    }]);
    setBindingMock('WorkflowListDefinitions', async () => ({
      baseBranch: 'main', predictedQueuePosition: 1, workflows: [],
    }));
    resetWorkflowEventStateForTest();
    for (const toast of getToasts()) removeToast(toast.id);
  });

  it('fans queue state into the pane and refreshes sidebar summaries', async () => {
    await initializeWorkflowsSidebar();
    applyWorkflowQueueStateEvent({ active: false, globalConcurrency: 3, startsRemaining: 2 });
    expect(getWorkflowQueueState()).toEqual({ active: false, globalConcurrency: 3, startsRemaining: 2 });
    await vi.waitFor(() => expect(getBindingMock('WorkflowListItems')).toHaveBeenCalledTimes(2));
  });

  it('fans item state into the always-on sidebar store', async () => {
    await initializeWorkflowsSidebar();
    applyWorkflowItemStateEvent({ itemId: 'run', from: 'running', to: 'needs-human', reason: 'gate' });
    expect(getProjectWorkflowAttentionCount('p')).toBe(1);
  });

  it('deduplicates user-facing errors by item and message', () => {
    applyWorkflowErrorEvent({ itemId: 'a', error: 'Provider stopped' });
    applyWorkflowErrorEvent({ itemId: 'a', error: 'Provider stopped' });
    applyWorkflowErrorEvent({ itemId: 'b', error: 'Provider stopped' });
    expect(getToasts().map((toast) => toast.message)).toEqual(['Provider stopped', 'Provider stopped']);
  });
});
