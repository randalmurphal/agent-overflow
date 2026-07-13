import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import type { Project } from '../../types/models';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { addProjectLocal, resetProjectsForTest } from '../../stores/projects.svelte';
import { pushWorkflowLevel, recordWorkflowReceipt, resetWorkflowsPane } from '../../stores/workflowsPane.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import WorkflowsPane from './WorkflowsPane.svelte';
import WorkflowAllClear from './WorkflowAllClear.svelte';

const project: Project = {
  id: 'p', name: 'Project', path: '/tmp/p', sortPosition: 0,
  createdAt: 1, updatedAt: 1, archived: false,
};

describe('WorkflowsPane navigation rendering', () => {
  beforeEach(() => {
    resetWorkflowsPane();
    resetPaneLayoutForTest();
    resetProjectsForTest();
    addProjectLocal(project);
    setPaneLayoutItemsForTest([{ id: 'workflows', paneId: 'workflows', kind: 'workflows', widthPx: 500 }]);
    setBindingMock('WorkflowListItems', async () => []);
    setBindingMock('WorkflowListItemCosts', async () => ({}));
    setBindingMock('WorkflowListDefinitions', async () => ({
      baseBranch: 'main', predictedQueuePosition: 1,
      workflows: [{ id: 'wf', name: 'Workflow', scope: 'shared', phaseCount: 0, phases: [], inputs: [], defaultStepMode: false, valid: true, allBindingsAvailable: true }],
    }));
  });

  it('renders overview then a pushed workflow level with back chrome', async () => {
    const view = render(WorkflowsPane, { paneId: 'workflows' });
    await waitFor(() => expect(view.getByTestId('wf-overview')).toBeInTheDocument());
    expect(view.getByTestId('wf-header')).toContainElement(view.getByTestId('wf-overview-controls'));
    pushWorkflowLevel({ kind: 'workflow', projectId: 'p', workflowId: 'wf', label: 'Workflow' });
    await waitFor(() => expect(view.getByTestId('wf-workflow-detail')).toBeInTheDocument());
    expect(view.getByTestId('wf-back')).toBeInTheDocument();
    expect(view.getByTestId('wf-title')).toHaveTextContent('Workflow');
    expect(view.queryByTestId('wf-overview-controls')).toBeNull();
  });

  it('blurs a focused field on Escape instead of popping the level', async () => {
    const view = render(WorkflowsPane, { paneId: 'workflows' });
    const filter = await view.findByTestId('wf-project-filter');
    (filter as HTMLSelectElement).focus();
    expect(document.activeElement).toBe(filter);
    await fireEvent.keyDown(filter, { key: 'Escape' });
    expect(document.activeElement).not.toBe(filter);
    expect(view.getByTestId('wf-overview')).toBeInTheDocument();
  });

  it('renders the all-clear receipt summary with per-action labels and no zero kinds', () => {
    recordWorkflowReceipt({ itemId: 'a', kind: 'approved', message: 'Approved', costUsd: 1 }, false);
    recordWorkflowReceipt({ itemId: 'b', kind: 'answered', message: 'Answered', costUsd: 2 }, false);
    recordWorkflowReceipt({ itemId: 'c', kind: 'handed-off', message: 'Handed off', costUsd: 3 }, false);
    recordWorkflowReceipt({ itemId: 'd', kind: 'merged', message: 'Merged', costUsd: 5.54 }, false);
    const view = render(WorkflowAllClear);
    expect(view.getByTestId('wf-all-clear')).toHaveTextContent(
      '4 resolved — 1 approved · 1 answered · 1 handed off · 1 merged · $11.54 reviewed',
    );
    expect(view.getByTestId('wf-all-clear')).not.toHaveTextContent('discarded');
  });
});
