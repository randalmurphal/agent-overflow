import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Project } from '../../types/models';
import { addProjectLocal, getProjects, resetProjectsForTest } from '../../stores/projects.svelte';
import { activateWorkflowsPane, getWorkflowDefinitions, getWorkflowError, loadWorkflowOverview, resetWorkflowsPane } from '../../stores/workflowsPane.svelte';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import WorkflowIntakeDialog from './WorkflowIntakeDialog.svelte';

const project: Project = {
  id: 'p', name: 'Project', path: '/tmp/p', sortPosition: 0,
  createdAt: 1, updatedAt: 1, archived: false,
};

describe('WorkflowIntakeDialog', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetProjectsForTest();
    resetWorkflowsPane();
    addProjectLocal(project);
    setBindingMock('WorkflowListItems', async () => []);
    setBindingMock('WorkflowListItemCosts', async () => ({}));
    setBindingMock('WorkflowListDefinitions', async () => ({
      baseBranch: 'main', predictedQueuePosition: 3,
      workflows: [{
        id: 'wf', name: 'Build', scope: 'shared', phaseCount: 1,
        phases: [{ id: 'build' }], defaultStepMode: true, valid: true,
        allBindingsAvailable: true,
        inputs: [
          { name: 'title', type: 'string', required: true },
          { name: 'mode', type: 'string', required: false, enum: ['fast', 'safe'] },
          { name: 'approved', type: 'boolean', required: false },
          { name: 'count', type: 'number', required: false },
          { name: 'source', type: 'string', required: false, format: 'path' },
        ],
      }],
    }));
    setBindingMock('WorkflowEnqueueItem', async () => ({ id: 'queued' }));
  });

  afterEach(resetBindingMocks);

  it('renders typed fields from the catalog fixture', async () => {
    expect(getProjects()).toHaveLength(1);
    activateWorkflowsPane();
    await loadWorkflowOverview();
    expect(getWorkflowError()).toBeNull();
    expect(getWorkflowDefinitions()).toHaveLength(1);
    const view = render(WorkflowIntakeDialog, { open: true, onClose: vi.fn() });
    await waitFor(() => expect(view.getByTestId('wf-intake-workflow')).toHaveTextContent('Build'));
    await view.getByTestId('wf-intake-workflow').click();
    await waitFor(() => expect(view.getByTestId('wf-seed-title')).toHaveAttribute('type', 'text'));
    expect(view.getByTestId('wf-seed-mode').tagName).toBe('SELECT');
    expect(view.getByTestId('wf-seed-approved')).toHaveAttribute('type', 'checkbox');
    expect(view.getByTestId('wf-seed-count')).toHaveAttribute('type', 'number');
    expect(view.getByTestId('wf-seed-source-pick')).toHaveTextContent('Browse');
    expect(view.getByTestId('wf-intake-base-branch')).toHaveValue('main');
    expect(view.getByTestId('wf-intake-step-mode')).toBeChecked();
    expect(view.getByTestId('wf-intake-submit')).toHaveTextContent('Queue — position 3');
  });

  it('gates required inputs and submits compact typed seeds', async () => {
    activateWorkflowsPane();
    await loadWorkflowOverview();
    const onClose = vi.fn();
    const view = render(WorkflowIntakeDialog, { open: true, onClose });
    await fireEvent.click(await view.findByTestId('wf-intake-workflow'));
    const submit = view.getByTestId('wf-intake-submit');
    expect(submit).toBeDisabled();
    await fireEvent.input(view.getByTestId('wf-intake-goal'), { target: { value: 'Ship it' } });
    await fireEvent.input(view.getByTestId('wf-seed-title'), { target: { value: 'Release' } });
    await fireEvent.change(view.getByTestId('wf-seed-mode'), { target: { value: 'safe' } });
    expect(submit).toBeEnabled();
    await fireEvent.click(submit);
    await waitFor(() => expect(getBindingMock('WorkflowEnqueueItem')).toHaveBeenCalledTimes(1));
    const args = getBindingMock('WorkflowEnqueueItem')!.mock.calls[0];
    expect([args[0], args[1], args[2], args[3], args[5], args[6]]).toEqual([
      'p', 'wf', 'shared', 'Ship it', null, true,
    ]);
    expect(JSON.parse(args[4] as string)).toEqual({ title: 'Release', mode: 'safe', approved: false });
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
