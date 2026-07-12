import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { Project } from '../../types/models';
import type { WorkItem } from '../../types/workflow';
import type { Settings } from '../../types/settings';
import { makeSettings } from '../../../test/helpers/settings';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetPaneLayoutForTest } from '../../stores/paneLayout.svelte';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { addProjectLocal, resetProjectsForTest } from '../../stores/projects.svelte';
import { resetSettingsForTest } from '../../stores/settings.svelte';
import {
  activateWorkflowsPane,
  getWorkflowStack,
  loadWorkflowOverview,
  resetWorkflowsPane,
} from '../../stores/workflowsPane.svelte';
import WorkflowOverview from './WorkflowOverview.svelte';
import { __resetRunModeForTest } from '../../transport/runMode';
import {
  getProjectWorkflowRuns,
  initializeWorkflowsSidebar,
  resetWorkflowsSidebarForTest,
} from '../../stores/workflowsSidebar.svelte';

const project: Project = {
  id: 'p', name: 'Project', path: '/tmp/p', sortPosition: 0,
  createdAt: 1, updatedAt: 1, archived: false,
};

const queued = {
  id: 'queued', projectId: 'p', goal: 'Queue me', workflowId: 'wf', workflowScope: 'shared',
  state: 'queued', reason: '', sortPosition: 0, stepMode: false, source: 'manual', createdAt: 1,
} as WorkItem;

describe('WorkflowOverview queue controls', () => {
  beforeEach(async () => {
    delete (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__;
    __resetRunModeForTest();
    resetBindingMocks();
    resetWorkflowsPane();
    resetWorkflowsSidebarForTest();
    resetSettingsForTest();
    resetPaneLayoutForTest();
    resetPanesForTest();
    resetProjectsForTest();
    addProjectLocal(project);
    setBindingMock('WorkflowListItems', async () => [queued]);
    setBindingMock('WorkflowListItemCosts', async () => ({ queued: 0.25 }));
    setBindingMock('WorkflowListDefinitions', async () => ({
      baseBranch: 'main', predictedQueuePosition: 1,
      workflows: [{ id: 'wf', name: 'Workflow', scope: 'shared', phaseCount: 0, phases: [], inputs: [], defaultStepMode: false, valid: true, allBindingsAvailable: true }],
    }));
    setBindingMock('UpdateSettings', async (patch: Partial<Settings>) => makeSettings(patch));
    setBindingMock('WorkflowCancelItem', async () => undefined);
    setBindingMock('WorkflowReorderQueue', async () => undefined);
    setBindingMock('WorkflowGetItem', async () => ({
      item: queued, phases: [], artifacts: [],
      usage: { inputTokens: 0, outputTokens: 0, totalTokens: 0, costUsd: 0 },
    }));
    activateWorkflowsPane();
    await loadWorkflowOverview();
  });

  afterEach(() => {
    delete (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__;
    __resetRunModeForTest();
    resetWorkflowsSidebarForTest();
    resetBindingMocks();
  });

  it('toggles through UpdateSettings and arms queued cancellation', async () => {
    const view = render(WorkflowOverview);
    await fireEvent.click(view.getByTestId('wf-queue-toggle'));
    expect(getBindingMock('UpdateSettings')).toHaveBeenCalledWith({ workflowQueueActive: false });
    expect(getBindingMock('WorkflowSetQueue')).toBeUndefined();

    const cancel = view.getByTestId('wf-queue-cancel');
    await fireEvent.click(cancel);
    expect(getBindingMock('WorkflowCancelItem')).not.toHaveBeenCalled();
    expect(cancel).toHaveTextContent('cancel?');
    await fireEvent.click(cancel);
    await waitFor(() => expect(getBindingMock('WorkflowCancelItem')).toHaveBeenCalledWith('queued'));
  });

  it('opens queued runs with the full overview-workflow-run stack', async () => {
    const view = render(WorkflowOverview);
    await fireEvent.click(view.getByTestId('wf-queue-open'));
    expect(getWorkflowStack().map((level) => level.kind)).toEqual(['overview', 'workflow', 'run']);
  });

  it('refreshes sidebar queue order after a successful drag reorder', async () => {
    const second = { ...queued, id: 'second', goal: 'Second', sortPosition: 1 } as WorkItem;
    let listed = [queued, second];
    setBindingMock('WorkflowListItems', async () => listed);
    setBindingMock('WorkflowReorderQueue', async (_projectId: string, ids: string[]) => {
      listed = ids.map((id, index) => ({ ...listed.find((item) => item.id === id)!, sortPosition: index }));
    });
    await loadWorkflowOverview();
    await initializeWorkflowsSidebar();
    const view = render(WorkflowOverview);
    const rows = view.getAllByTestId('wf-queue-row');

    await fireEvent.dragStart(rows[1]);
    await fireEvent.drop(rows[0]);

    await waitFor(() => expect(getBindingMock('WorkflowReorderQueue')).toHaveBeenCalledWith('p', ['second', 'queued']));
    await waitFor(() => expect(getProjectWorkflowRuns('p').map((item) => item.id)).toEqual(['second', 'queued']));
  });

  it('disables every overview mutation while keeping run navigation live remotely', async () => {
    (globalThis as { __AO_BOOTSTRAP__?: { remote: boolean } }).__AO_BOOTSTRAP__ = { remote: true };
    __resetRunModeForTest();
    const view = render(WorkflowOverview);

    for (const testId of ['wf-queue-toggle', 'wf-new-run', 'wf-new-workflow', 'wf-triage', 'wf-queue-cancel']) {
      const control = view.getByTestId(testId) as HTMLButtonElement;
      expect(control.disabled).toBe(true);
      expect(control.title).toBe('Local only');
    }
    expect(view.getByTestId('wf-queue-row').getAttribute('draggable')).toBe('false');
    expect(view.queryByTestId('wf-queue-grip')).toBeNull();
    expect((view.getByTestId('wf-queue-open') as HTMLButtonElement).disabled).toBe(false);
  });
});
