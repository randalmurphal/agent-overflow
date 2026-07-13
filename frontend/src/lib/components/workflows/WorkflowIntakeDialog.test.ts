import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Project } from '../../types/models';
import { addProjectLocal, getProjects, resetProjectsForTest } from '../../stores/projects.svelte';
import { activateWorkflowsPane, getWorkflowDefinitions, getWorkflowError, loadWorkflowOverview, openWorkflowIntake, resetWorkflowsPane, setWorkflowProjectFilter } from '../../stores/workflowsPane.svelte';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import WorkflowIntakeDialog from './WorkflowIntakeDialog.svelte';
import { __resetRunModeForTest } from '../../transport/runMode';
import {
  getProjectWorkflowRuns,
  initializeWorkflowsSidebar,
  resetWorkflowsSidebarForTest,
} from '../../stores/workflowsSidebar.svelte';
import type { WorkItem } from '../../types/workflow';

const project: Project = {
  id: 'p', name: 'Project', path: '/tmp/p', sortPosition: 0,
  createdAt: 1, updatedAt: 1, archived: false,
};

describe('WorkflowIntakeDialog', () => {
  beforeEach(() => {
    delete (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__;
    __resetRunModeForTest();
    resetBindingMocks();
    resetProjectsForTest();
    resetWorkflowsPane();
    resetWorkflowsSidebarForTest();
    addProjectLocal(project);
    setBindingMock('WorkflowListItems', async () => []);
    setBindingMock('WorkflowListUnresolvedItems', async () => []);
    setBindingMock('WorkflowListItemCosts', async () => ({}));
    setBindingMock('WorkflowListDefinitions', async () => ({
      baseBranch: 'main', predictedQueuePosition: 3,
      workflows: [{
        id: 'wf', name: 'Build', scope: 'shared', phaseCount: 1, humanGateCount: 1,
        phases: [{ id: 'build' }], defaultStepMode: true, valid: true,
        allBindingsAvailable: true,
        inputs: [
          { name: 'title', type: 'string', required: true },
          { name: 'mode', type: 'string', required: false, enum: ['fast', 'safe'] },
          { name: 'approved', type: 'boolean', required: false },
          { name: 'count', type: 'number', required: false },
          { name: 'notes', type: 'string', required: false, multiline: true },
          { name: 'source', type: 'string', required: false, format: 'path' },
        ],
      }],
    }));
    setBindingMock('WorkflowEnqueueItem', async () => ({ id: 'queued' }));
    setBindingMock('WorkflowQueueChatProposal', async () => ({ id: 'queued-proposal' }));
    setBindingMock('BrowseDirectory', async () => ({
      path: '/tmp/p', parent: '/tmp', separator: '/', exists: true, truncated: false,
      entries: [{ name: 'brief.md', isDir: false, hidden: false, isRepo: false }],
    }));
  });

  afterEach(() => {
    delete (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__;
    __resetRunModeForTest();
    resetWorkflowsSidebarForTest();
    resetBindingMocks();
  });

  it('renders typed fields from the catalog fixture', async () => {
    expect(getProjects()).toHaveLength(1);
    activateWorkflowsPane();
    await loadWorkflowOverview();
    expect(getWorkflowError()).toBeNull();
    expect(getWorkflowDefinitions()).toHaveLength(1);
    const view = render(WorkflowIntakeDialog, { open: true, onClose: vi.fn() });
    await waitFor(() => expect(view.getByTestId('wf-intake-workflow')).toHaveTextContent('Build'));
    expect(view.getByTestId('wf-intake-workflow')).toHaveTextContent('1 phase · 1 human gate');
    await view.getByTestId('wf-intake-workflow').click();
    await waitFor(() => expect(view.getByTestId('wf-seed-title')).toHaveAttribute('type', 'text'));
    expect(view.getByTestId('wf-seed-mode').tagName).toBe('SELECT');
    expect(view.getByTestId('wf-seed-approved')).toHaveAttribute('type', 'checkbox');
    expect(view.getByTestId('wf-seed-count')).toHaveAttribute('type', 'number');
    expect(view.getByTestId('wf-seed-notes').tagName).toBe('TEXTAREA');
    expect(view.getByTestId('wf-seed-notes').closest('label')?.querySelector('.text-fg-hint')).toHaveTextContent('(optional)');
    expect(view.getByTestId('wf-seed-source-pick')).toHaveTextContent('Browse');
    expect(view.getByTestId('wf-intake-base-branch')).toHaveValue('main');
    expect(view.getByTestId('wf-intake-step-mode')).toBeChecked();
    expect(view.getByTestId('wf-intake-submit')).toHaveTextContent('Queue — position 3');
  });

  it('commits a selected file into a path seed', async () => {
    activateWorkflowsPane();
    await loadWorkflowOverview();
    const view = render(WorkflowIntakeDialog, { open: true, onClose: vi.fn() });
    await fireEvent.click(await view.findByTestId('wf-intake-workflow'));
    await fireEvent.click(view.getByTestId('wf-seed-source-pick'));
    const entry = await view.findByTestId('directory-browser-entry');
    await fireEvent.click(entry);
    expect(view.getByTestId('wf-seed-source')).toHaveValue('/tmp/p/brief.md');
    expect(view.queryByTestId('directory-browser-list')).toBeNull();
  });

  it('keeps committing browsed directories into a path seed', async () => {
    activateWorkflowsPane();
    await loadWorkflowOverview();
    const view = render(WorkflowIntakeDialog, { open: true, onClose: vi.fn() });
    await fireEvent.click(await view.findByTestId('wf-intake-workflow'));
    await fireEvent.click(view.getByTestId('wf-seed-source-pick'));
    await view.findByTestId('directory-browser-entry');
    expect(view.getByTestId('wf-seed-source')).toHaveValue('/tmp/p');
    expect(view.getByTestId('directory-browser-list')).toBeInTheDocument();
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
    await fireEvent.input(view.getByTestId('wf-intake-base-branch'), { target: { value: 'release/v2' } });
    expect(submit).toBeEnabled();
    await fireEvent.click(submit);
    await waitFor(() => expect(getBindingMock('WorkflowEnqueueItem')).toHaveBeenCalledTimes(1));
    const args = getBindingMock('WorkflowEnqueueItem')!.mock.calls[0];
    expect([args[0], args[1], args[2], args[3], args[5], args[6], args[7]]).toEqual([
      'p', 'wf', 'shared', 'Ship it', null, 'release/v2', true,
    ]);
    // The wire carries seeds as one JSON object; a stringified payload arrives
    // as a JSON string literal and the engine rejects it.
    expect(args[4]).toEqual({ title: 'Release', mode: 'safe', approved: false });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('loads workflows for the dialog project independently of the pane filter', async () => {
    addProjectLocal({ ...project, id: 'other', name: 'Other', path: '/tmp/other' });
    setBindingMock('WorkflowListDefinitions', async (projectId: string) => ({
      baseBranch: `${projectId}-base`, predictedQueuePosition: 1,
      workflows: [{
        id: `${projectId}-workflow`, name: `${projectId} workflow`, scope: 'shared', phaseCount: 0,
        phases: [], inputs: [], defaultStepMode: false, valid: true, allBindingsAvailable: true,
      }],
    }));
    activateWorkflowsPane();
    setWorkflowProjectFilter('p');
    openWorkflowIntake({ projectId: 'p', baseBranch: 'prefilled-base' });
    const view = render(WorkflowIntakeDialog, { open: true, onClose: vi.fn() });
    await waitFor(() => expect(view.getByTestId('wf-intake-workflow')).toHaveTextContent('p workflow'));
    await fireEvent.click(view.getByTestId('wf-intake-workflow'));
    expect(view.getByTestId('wf-intake-base-branch')).toHaveValue('prefilled-base');

    await fireEvent.click(view.getAllByTestId('wf-intake-project').find((button) => button.textContent?.includes('Other'))!);
    await waitFor(() => expect(view.getByTestId('wf-intake-workflow')).toHaveTextContent('other workflow'));
    await fireEvent.click(view.getByTestId('wf-intake-workflow'));
    expect(view.getByTestId('wf-intake-base-branch')).toHaveValue('other-base');
  });

  it('resolves an edited chat proposal instead of creating a manual run', async () => {
    activateWorkflowsPane();
    await loadWorkflowOverview();
    openWorkflowIntake({
      threadId: 'thread-1', proposalId: 'proposal-1', projectId: 'p',
      workflowId: 'wf', goal: 'Edit me', seeds: { title: 'Release' }, baseBranch: 'main',
    });
    const view = render(WorkflowIntakeDialog, { open: true, onClose: vi.fn() });
    await waitFor(() => expect(view.getByTestId('wf-intake-workflow')).toHaveTextContent('Build'));
    await waitFor(() => expect(view.getByTestId('wf-intake-submit')).toBeEnabled());
    await fireEvent.click(view.getByTestId('wf-intake-submit'));
    await waitFor(() => expect(getBindingMock('WorkflowQueueChatProposal')).toHaveBeenCalled());
    expect(getBindingMock('WorkflowQueueChatProposal')!.mock.calls[0].slice(0, 6)).toEqual([
      'thread-1', 'proposal-1', 'p', 'wf', 'shared', 'Edit me',
    ]);
    expect(getBindingMock('WorkflowEnqueueItem')).not.toHaveBeenCalled();
  });

  it('refuses intake submission in a view-only session', async () => {
    (globalThis as { __AO_BOOTSTRAP__?: { remote: boolean } }).__AO_BOOTSTRAP__ = { remote: true };
    __resetRunModeForTest();
    activateWorkflowsPane();
    await loadWorkflowOverview();
    const view = render(WorkflowIntakeDialog, { open: true, onClose: vi.fn() });
    const submit = await view.findByTestId('wf-intake-submit');
    expect(submit).toBeDisabled();
    expect(submit).toHaveAttribute('title', 'Local only');
    await fireEvent.submit(view.getByTestId('wf-intake-dialog'));
    expect(getBindingMock('WorkflowEnqueueItem')).not.toHaveBeenCalled();
  });

  it('refreshes the independent sidebar after enqueue while the queue is paused', async () => {
    let listedItems: WorkItem[] = [];
    setBindingMock('WorkflowListItems', async () => listedItems);
    setBindingMock('WorkflowListUnresolvedItems', async () => listedItems);
    activateWorkflowsPane();
    await loadWorkflowOverview();
    await initializeWorkflowsSidebar();
    const view = render(WorkflowIntakeDialog, { open: true, onClose: vi.fn() });
    await fireEvent.click(await view.findByTestId('wf-intake-workflow'));
    await fireEvent.input(view.getByTestId('wf-intake-goal'), { target: { value: 'Ship it' } });
    await fireEvent.input(view.getByTestId('wf-seed-title'), { target: { value: 'Release' } });
    listedItems = [{
      id: 'queued', projectId: 'p', workflowId: 'wf', workflowScope: 'shared', goal: 'Ship it',
      state: 'queued', reason: '', sortPosition: 1, stepMode: false, source: 'manual', createdAt: 1,
    } as WorkItem];
    await fireEvent.click(view.getByTestId('wf-intake-submit'));
    await waitFor(() => expect(getProjectWorkflowRuns('p').map((item) => item.id)).toEqual(['queued']));
  });
});
