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
  applyWorkflowQueueState,
  getWorkflowStack,
  loadWorkflowOverview,
  resetWorkflowsPane,
} from '../../stores/workflowsPane.svelte';
import WorkflowOverview from './WorkflowOverview.svelte';
import WorkflowOverviewControls from './WorkflowOverviewControls.svelte';
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
      workflows: [{ id: 'wf', name: 'Workflow', scope: 'shared', phaseCount: 1, humanGateCount: 1, phases: [{ id: 'review' }], inputs: [], defaultStepMode: false, valid: true, allBindingsAvailable: true }],
    }));
    setBindingMock('UpdateSettings', async (patch: Partial<Settings>) => makeSettings(patch));
    setBindingMock('WorkflowRemoveQueuedItem', async () => undefined);
    setBindingMock('WorkflowReorderQueue', async () => undefined);
    setBindingMock('WorkflowUpdateProjectQueue', async () => undefined);
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
    const controls = render(WorkflowOverviewControls);
    await fireEvent.click(controls.getByTestId('wf-queue-toggle'));
    expect(getBindingMock('UpdateSettings')).toHaveBeenCalledWith({ workflowQueueActive: false });
    expect(getBindingMock('WorkflowSetQueue')).toBeUndefined();

    const view = render(WorkflowOverview);
    const cancel = view.getByTestId('wf-queue-cancel');
    await fireEvent.click(cancel);
    expect(getBindingMock('WorkflowRemoveQueuedItem')).not.toHaveBeenCalled();
    expect(cancel).toHaveTextContent('cancel?');
    await fireEvent.click(cancel);
    await waitFor(() => expect(getBindingMock('WorkflowRemoveQueuedItem')).toHaveBeenCalledWith('queued'));
  });

  it('renders server-owned slot usage in the header controls', () => {
    applyWorkflowQueueState({
      active: true, globalConcurrency: 4, runningCount: 2, slotCapacity: 4,
    });
    const view = render(WorkflowOverviewControls);
    expect(view.getByTestId('wf-slots')).toHaveTextContent('2/4');
    expect(view.getByTestId('wf-slots')).toHaveAttribute('title', '2 of 4 concurrency slots in use');
    expect(view.getByTestId('wf-overflow')).toBeInTheDocument();
  });

  it('opens queued runs with the full overview-workflow-run stack', async () => {
    const view = render(WorkflowOverview);
    expect(view.getByTestId('wf-workflow-row')).toHaveTextContent('1 phase · 1 human gate');
    expect(view.getByTestId('wf-queue-row')).toHaveTextContent('#1');
    expect(view.getByTestId('wf-queue-row')).toHaveTextContent('Workflow · queued');
    await fireEvent.click(view.getByTestId('wf-queue-open'));
    expect(getWorkflowStack().map((level) => level.kind)).toEqual(['overview', 'workflow', 'run']);
  });

  it('shows the latest terminal run age in an idle workflow aggregate', async () => {
    const endedAt = Date.now() - 3 * 24 * 60 * 60 * 1_000;
    setBindingMock('WorkflowListItems', async () => [{
      ...queued, id: 'landed', state: 'done', disposition: { action: 'merged', policy: 'manual', at: endedAt },
      createdAt: endedAt - 1000, endedAt,
    }] as WorkItem[]);
    await loadWorkflowOverview();
    const view = render(WorkflowOverview);
    expect(view.getByTestId('wf-section-idle')).toHaveTextContent('idle · last run 3d ago');
  });

  it('shows automation spawn age and held state in queued metadata', async () => {
    setBindingMock('WorkflowListItems', async () => [{
      ...queued, source: 'automation', createdAt: Date.now() - 6 * 60 * 1000,
    }] as WorkItem[]);
    await loadWorkflowOverview();
    applyWorkflowQueueState({ active: false, globalConcurrency: 2, runningCount: 0, slotCapacity: 2 });
    const view = render(WorkflowOverview);
    expect(view.getByTestId('wf-queue-row')).toHaveTextContent('Workflow · spawned 6m ago · held');
  });

  it('explains that direct removal of an automation run is temporary', async () => {
    setBindingMock('WorkflowListItems', async () => [{ ...queued, source: 'automation' }] as WorkItem[]);
    await loadWorkflowOverview();
    const view = render(WorkflowOverview);
    const cancel = view.getByTestId('wf-queue-cancel');
    await fireEvent.click(cancel);
    await fireEvent.click(cancel);
    await waitFor(() => expect(getBindingMock('WorkflowRemoveQueuedItem')).toHaveBeenCalledWith('queued'));
    expect(getToasts().some((toast) => toast.message === 'Removed from queue — automation will re-propose it next cycle')).toBe(true);
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

  it('groups projects independently and supports dropping after the last row', async () => {
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
    const groups = view.getAllByTestId('wf-project-queue');
    expect(groups.map((group) => group.querySelector('[data-testid="wf-project-queue-name"]')?.textContent)).toEqual(['Other', 'Project']);
    let rows = view.getAllByTestId('wf-queue-row');

    await fireEvent.dragStart(rows.find((row) => row.textContent?.includes('Queue me'))!);
    await fireEvent.drop(rows.find((row) => row.textContent?.includes('Other'))!);
    expect(getBindingMock('WorkflowReorderQueue')).not.toHaveBeenCalled();

    rows = view.getAllByTestId('wf-queue-row');
    await fireEvent.dragStart(rows.find((row) => row.textContent?.includes('Queue me'))!);
    const afterLast = await view.findByTestId('wf-queue-drop-after');
    await fireEvent.dragOver(afterLast);
    await fireEvent.drop(afterLast);
    await waitFor(() => expect(getBindingMock('WorkflowReorderQueue')).toHaveBeenCalledWith('p', ['second', 'queued']));
  });

  it('updates project pause and concurrency controls and renders effective slots', async () => {
    applyWorkflowQueueState({
      active: true, globalConcurrency: 4, runningCount: 1, slotCapacity: 4,
      projects: [{ projectId: 'p', paused: false, concurrency: 2, runningCount: 1 }],
    });
    const view = render(WorkflowOverview);
    expect(view.getByTestId('wf-project-queue-name')).toHaveTextContent('Project');
    expect(view.getByTestId('wf-project-slots')).toHaveTextContent('1/2');

    await fireEvent.click(view.getByTestId('wf-project-queue-toggle'));
    await waitFor(() => expect(getBindingMock('WorkflowUpdateProjectQueue')).toHaveBeenCalledWith('p', true, null));
    expect(view.getByTestId('wf-project-queue-toggle')).toHaveTextContent('Resume');
    expect(view.getByTestId('wf-queue-row')).toHaveTextContent('held');

    await fireEvent.change(view.getByTestId('wf-project-concurrency'), { target: { value: '3' } });
    await waitFor(() => expect(getBindingMock('WorkflowUpdateProjectQueue')).toHaveBeenCalledWith('p', null, 3));
    expect(view.getByTestId('wf-project-slots')).toHaveTextContent('1/3');
  });

  it('renders a running-only project group', async () => {
    setBindingMock('WorkflowListItems', async () => [{ ...queued, state: 'running' }] as WorkItem[]);
    await loadWorkflowOverview();
    applyWorkflowQueueState({
      active: true, globalConcurrency: 2,
      projects: [{ projectId: 'p', paused: false, concurrency: 0, runningCount: 1 }],
    });
    const view = render(WorkflowOverview);
    expect(view.getByTestId('wf-project-queue')).toBeInTheDocument();
    expect(view.getByTestId('wf-project-slots')).toHaveTextContent('1/2');
    expect(view.queryByTestId('wf-queue-row')).toBeNull();
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
    const controls = render(WorkflowOverviewControls);
    const view = render(WorkflowOverview);

    for (const testId of ['wf-queue-toggle', 'wf-new-run', 'wf-new-workflow', 'wf-triage', 'wf-queue-cancel', 'wf-project-queue-toggle']) {
      const owner = testId === 'wf-queue-cancel' || testId === 'wf-project-queue-toggle' ? view : controls;
      const control = owner.getByTestId(testId) as HTMLButtonElement;
      expect(control.disabled).toBe(true);
      expect(control.title).toBe('Local only');
    }
    const concurrency = view.getByTestId('wf-project-concurrency') as HTMLSelectElement;
    expect(concurrency.disabled).toBe(true);
    expect(concurrency.title).toBe('Local only');
    expect(view.getByTestId('wf-queue-row').getAttribute('draggable')).toBe('false');
    expect(view.getByTestId('wf-queue-grip')).toHaveAttribute('title', 'Local only');
    expect(view.getByTestId('wf-queue-grip')).toHaveAttribute('aria-disabled', 'true');
    expect((view.getByTestId('wf-queue-open') as HTMLButtonElement).disabled).toBe(false);
  });
});
