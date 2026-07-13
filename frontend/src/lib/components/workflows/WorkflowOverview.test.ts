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
import { getToasts, removeToast } from '../../stores/toast.svelte';

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
    for (const toast of getToasts()) removeToast(toast.id);
    setBindingMock('WorkflowListItems', async () => [queued]);
    setBindingMock('WorkflowListUnresolvedItems', async () => [queued]);
    setBindingMock('WorkflowListItemCosts', async () => ({ queued: 0.25 }));
    setBindingMock('WorkflowListDefinitions', async () => ({
      baseBranch: 'main', predictedQueuePosition: 1,
      workflows: [{ id: 'wf', name: 'Workflow', scope: 'shared', phaseCount: 0, phases: [], inputs: [], defaultStepMode: false, valid: true, allBindingsAvailable: true }],
    }));
    setBindingMock('UpdateSettings', async (patch: Partial<Settings>) => makeSettings(patch));
    setBindingMock('WorkflowRemoveQueuedItem', async () => undefined);
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
    expect(getBindingMock('WorkflowRemoveQueuedItem')).not.toHaveBeenCalled();
    expect(cancel).toHaveTextContent('cancel?');
    await fireEvent.click(cancel);
    await waitFor(() => expect(getBindingMock('WorkflowRemoveQueuedItem')).toHaveBeenCalledWith('queued'));
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
    setBindingMock('WorkflowListUnresolvedItems', async () => listed);
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

  it('explains cross-project drag refusal and supports dropping after the last row', async () => {
    addProjectLocal({ ...project, id: 'other', name: 'Other', path: '/tmp/other' });
    let listed = [
      queued,
      { ...queued, id: 'second', goal: 'Second', sortPosition: 1 },
      { ...queued, id: 'other', projectId: 'other', goal: 'Other', sortPosition: 0 },
    ] as WorkItem[];
    setBindingMock('WorkflowListItems', async (projectId: string) => listed.filter((item) => item.projectId === projectId));
    setBindingMock('WorkflowReorderQueue', async (projectId: string, ids: string[]) => {
      const byId = new Map(listed.map((item) => [item.id, item]));
      listed = listed.map((item) => item.projectId === projectId
        ? { ...byId.get(ids.find((id) => id === item.id)!)! }
        : item);
    });
    await loadWorkflowOverview();
    const view = render(WorkflowOverview);
    let rows = view.getAllByTestId('wf-queue-row');

    await fireEvent.dragStart(rows.find((row) => row.textContent?.includes('Queue me'))!);
    await fireEvent.drop(rows.find((row) => row.textContent?.includes('Other'))!);
    expect(getToasts().some((toast) => toast.message.includes('per project'))).toBe(true);
    expect(getBindingMock('WorkflowReorderQueue')).not.toHaveBeenCalled();

    rows = view.getAllByTestId('wf-queue-row');
    await fireEvent.dragStart(rows.find((row) => row.textContent?.includes('Queue me'))!);
    const afterLast = await view.findByTestId('wf-queue-drop-after');
    await fireEvent.dragOver(afterLast);
    await fireEvent.drop(afterLast);
    await waitFor(() => expect(getBindingMock('WorkflowReorderQueue')).toHaveBeenCalledWith('p', ['second', 'queued']));
  });

  it('guards queued cancellation and reorder while each request is in flight', async () => {
    let releaseCancel!: () => void;
    setBindingMock('WorkflowRemoveQueuedItem', () => new Promise<void>((resolve) => { releaseCancel = resolve; }));
    const second = { ...queued, id: 'second', goal: 'Second', sortPosition: 1 } as WorkItem;
    setBindingMock('WorkflowListItems', async () => [queued, second]);
    let releaseReorder!: () => void;
    setBindingMock('WorkflowReorderQueue', () => new Promise<void>((resolve) => { releaseReorder = resolve; }));
    await loadWorkflowOverview();
    const view = render(WorkflowOverview);

    const cancel = view.getAllByTestId('wf-queue-cancel')[0];
    await fireEvent.click(cancel);
    await fireEvent.click(cancel);
    await waitFor(() => expect(getBindingMock('WorkflowRemoveQueuedItem')).toHaveBeenCalledTimes(1));
    expect(cancel).toBeDisabled();
    await fireEvent.click(cancel);
    expect(getBindingMock('WorkflowRemoveQueuedItem')).toHaveBeenCalledTimes(1);
    releaseCancel();

    const rows = view.getAllByTestId('wf-queue-row');
    await fireEvent.dragStart(rows[1]);
    await fireEvent.drop(rows[0]);
    await waitFor(() => expect(getBindingMock('WorkflowReorderQueue')).toHaveBeenCalledTimes(1));
    expect(view.getAllByTestId('wf-queue-row')[0]).toHaveAttribute('draggable', 'false');
    releaseReorder();
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
