import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { addProjectLocal, resetProjectsForTest } from '../../stores/projects.svelte';
import {
  activateWorkflowsPane,
  loadWorkflowCurrentLevel,
  pushWorkflowLevel,
  resetWorkflowsPane,
} from '../../stores/workflowsPane.svelte';
import { resetPaneLayoutForTest } from '../../stores/paneLayout.svelte';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { __resetRunModeForTest } from '../../transport/runMode';
import { openReviewCompanion } from '../../stores/reviewPane.svelte';
import WorkflowRunDetail from './WorkflowRunDetail.svelte';

vi.mock('../../stores/reviewPane.svelte', () => ({
  openReviewCompanion: vi.fn(async () => null),
}));

const level = {
  kind: 'run' as const,
  projectId: 'p',
  workflowId: 'wf',
  workflowLabel: 'Build',
  itemId: 'run',
  label: 'Run',
  sweep: false,
};

describe('<WorkflowRunDetail>', () => {
  beforeEach(async () => {
    resetBindingMocks();
    resetWorkflowsPane();
    resetPaneLayoutForTest();
    resetPanesForTest();
    resetProjectsForTest();
    delete (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__;
    __resetRunModeForTest();
    vi.mocked(openReviewCompanion).mockClear();
    addProjectLocal({ id: 'p', name: 'Project', path: '/tmp/p', sortPosition: 0, createdAt: 1, updatedAt: 1, archived: false });
    setBindingMock('WorkflowListItems', async () => [{
      id: 'run', projectId: 'p', workflowId: 'wf', goal: 'Run', state: 'done', reason: '',
      baseBranch: 'main', worktreePath: '/tmp/run', sortPosition: 0, createdAt: 1,
    }]);
    setBindingMock('WorkflowListItemCosts', async () => ({}));
    setBindingMock('WorkflowListDefinitions', async () => ({
      baseBranch: 'main', predictedQueuePosition: 1,
      workflows: [{ id: 'wf', name: 'Build', scope: 'shared', phaseCount: 2, phases: [], inputs: [], defaultStepMode: false, valid: true, allBindingsAvailable: true }],
    }));
    setBindingMock('WorkflowGetItem', async () => ({
      item: {
        id: 'run', projectId: 'p', workflowId: 'wf', goal: 'Run', state: 'done', reason: '',
        baseBranch: 'main', worktreePath: '/tmp/run', sortPosition: 0, createdAt: 1,
      },
      phases: [
        { phaseId: 'plan', attempt: 1, status: 'completed', threadId: 'old', startedAt: 1, endedAt: 2 },
        { phaseId: 'build', attempt: 1, status: 'completed', threadId: 'newest', startedAt: 2, endedAt: 3 },
      ],
      artifacts: [],
      usage: { inputTokens: 0, outputTokens: 0, totalTokens: 0, costUsd: 0 },
    }));
    activateWorkflowsPane();
    pushWorkflowLevel({ kind: 'workflow', projectId: 'p', workflowId: 'wf', label: 'Build' });
    pushWorkflowLevel(level);
    await loadWorkflowCurrentLevel();
  });

  afterEach(() => {
    delete (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__;
    __resetRunModeForTest();
    resetBindingMocks();
  });

  it('opens the real review companion against the newest phase thread', async () => {
    const view = render(WorkflowRunDetail, { level });
    const button = await view.findByTestId('wf-open-full-review');
    await fireEvent.click(button);
    expect(openReviewCompanion).toHaveBeenCalledWith('workflows', 'newest', {
      scope: 'branch', baseBranch: 'main', workspacePath: '/tmp/run',
    });
  });

  it('renders local diff affordances disabled remotely', async () => {
    (globalThis as { __AO_BOOTSTRAP__?: { remote: boolean } }).__AO_BOOTSTRAP__ = { remote: true };
    __resetRunModeForTest();
    const view = render(WorkflowRunDetail, { level });
    await waitFor(() => expect(view.getByTestId('wf-diff-load')).toBeInTheDocument());
    for (const testId of ['wf-diff-load', 'wf-open-full-review']) {
      const button = view.getByTestId(testId) as HTMLButtonElement;
      expect(button.disabled).toBe(true);
      expect(button.title).toBe('Local only');
    }
  });
});
