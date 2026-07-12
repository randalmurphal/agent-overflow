import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import WorkflowsFooter from '../WorkflowsFooter.svelte';
import { resetBindingMocks, setBindingMock } from '../../../../test/mocks/bindings-app';
import { refreshProjects, resetProjectsForTest } from '../../../stores/projects.svelte';
import { resetWorkflowsSidebarForTest } from '../../../stores/workflowsSidebar.svelte';
import { getWorkflowCurrentLevel, resetWorkflowsPane } from '../../../stores/workflowsPane.svelte';
import { resetPaneLayoutForTest } from '../../../stores/paneLayout.svelte';
import { resetPanesForTest } from '../../../stores/panes.svelte';

describe('<WorkflowsFooter>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetProjectsForTest();
    resetWorkflowsSidebarForTest();
    resetWorkflowsPane();
    resetPaneLayoutForTest();
    resetPanesForTest();
    setBindingMock('ListProjects', async () => [{
      project: { id: 'p', name: 'Project', path: '/tmp/p', sortPosition: 0, createdAt: 1, updatedAt: 1, archived: false },
      threadCount: 0, lastActive: 0,
    }]);
    setBindingMock('WorkflowListItems', async () => [{
      id: 'run', projectId: 'p', workflowId: 'wf', goal: 'Run', state: 'failed', sortPosition: 1, createdAt: 1,
    }]);
    setBindingMock('WorkflowListDefinitions', async () => ({ baseBranch: 'main', predictedQueuePosition: 1, workflows: [] }));
  });

  afterEach(() => resetBindingMocks());

  it('initializes after projects hydrate and opens the overview', async () => {
    const view = render(WorkflowsFooter);
    await refreshProjects();
    await waitFor(() => expect(view.getByTestId('workflows-footer-attention').textContent).toBe('1'));
    await fireEvent.click(view.getByTestId('sidebar-workflows-button'));
    expect(getWorkflowCurrentLevel()).toEqual({ kind: 'overview' });
  });

  it('always renders without a badge when quiet', async () => {
    setBindingMock('WorkflowListItems', async () => []);
    const view = render(WorkflowsFooter);
    await refreshProjects();
    await waitFor(() => expect(view.getByTestId('workflows-footer')).toBeInTheDocument());
    expect(view.queryByTestId('workflows-footer-attention')).toBeNull();
  });
});
