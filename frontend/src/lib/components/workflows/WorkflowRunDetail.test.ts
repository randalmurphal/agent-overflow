import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import type { WorkflowItemDetail } from '../../types/workflow';
import { addProjectLocal, resetProjectsForTest } from '../../stores/projects.svelte';
import {
  activateWorkflowsPane,
  getWorkflowDiffFiles,
  loadWorkflowOverview,
  loadWorkflowCurrentLevel,
  pushWorkflowLevel,
  recordWorkflowReceipt,
  resetWorkflowsPane,
} from '../../stores/workflowsPane.svelte';
import { resetPaneLayoutForTest } from '../../stores/paneLayout.svelte';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { __resetRunModeForTest } from '../../transport/runMode';
import { openReviewCompanion } from '../../stores/reviewPane.svelte';
import WorkflowRunDetail from './WorkflowRunDetail.svelte';
import WorkflowRunHeader from './WorkflowRunHeader.svelte';

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
      workflows: [{ id: 'wf', name: 'Build', scope: 'shared', phaseCount: 2, phases: [{ id: 'plan' }, { id: 'build' }], inputs: [], defaultStepMode: false, valid: true, allBindingsAvailable: true }],
    }));
    setBindingMock('WorkflowGetItem', async () => ({
      item: {
        id: 'run', projectId: 'p', workflowId: 'wf', goal: 'Run', state: 'done', reason: '',
        baseBranch: 'main', worktreePath: '/tmp/run', sortPosition: 0, createdAt: 1, endedAt: Date.now() - 2 * 60 * 60 * 1000,
      },
      phases: [
        { phaseId: 'plan', attempt: 1, status: 'completed', threadId: 'old', startedAt: 1, endedAt: 2 },
        { phaseId: 'build', attempt: 1, status: 'completed', threadId: 'newest', startedAt: 2, endedAt: 3 },
      ],
      checkPhaseIds: ['build'],
      outputs: {},
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

  it('renders checks from the server-projected check phase ids', async () => {
    const view = render(WorkflowRunDetail, { level });
    expect(await view.findByTestId('wf-checks')).toHaveTextContent('build');
    expect(view.getByTestId('wf-checks')).not.toHaveTextContent('plan');
  });

  it('renders the project, state, phase ordinal, finished age, and cost header rows', async () => {
    const view = render(WorkflowRunDetail, { level });
    expect(await view.findByTestId('wf-run-header')).toHaveTextContent('Project');
    expect(view.getByTestId('wf-run-state')).toHaveTextContent('Done');
    expect(view.getByTestId('wf-run-hint')).toHaveTextContent('Build · phase 2/2 · finished 2h ago · $0.00');
  });

  it('renders sweep count, current/resolved progress dots, and j/k hints', async () => {
    setBindingMock('WorkflowListItems', async () => [
      { id: 'run', projectId: 'p', workflowId: 'wf', goal: 'Run', state: 'needs-human', reason: 'gate', sortPosition: 0, createdAt: 1 },
      { id: 'other', projectId: 'p', workflowId: 'wf', goal: 'Other', state: 'failed', reason: 'agent-error', sortPosition: 1, createdAt: 2 },
    ]);
    await loadWorkflowOverview();
    recordWorkflowReceipt({ itemId: 'other', kind: 'handed-off', message: 'Handed off', costUsd: 0 }, false);
    const sweepLevel = { ...level, sweep: true };
    const detail = {
      item: { id: 'run', projectId: 'p', workflowId: 'wf', goal: 'Run', state: 'needs-human', reason: 'gate', createdAt: 1 },
      phases: [{ phaseId: 'build', attempt: 1, status: 'completed', startedAt: 1, endedAt: 2 }],
      checkPhaseIds: [], outputs: {}, artifacts: [], usage: { inputTokens: 0, outputTokens: 0, totalTokens: 0, costUsd: 0 },
    } as unknown as WorkflowItemDetail;
    const view = render(WorkflowRunHeader, { detail, level: sweepLevel, projectName: 'Project' });
    expect(view.getByTestId('wf-sweep-counter')).toHaveTextContent('1 of 2');
    const dots = view.getByTestId('wf-sweep-progress').children;
    expect(dots).toHaveLength(2);
    expect(dots[0]).toHaveClass('ring-accent');
    expect(dots[1]).toHaveClass('bg-success');
    expect(view.getByTestId('wf-sweep-prev')).toHaveTextContent('k');
    expect(view.getByTestId('wf-sweep-next')).toHaveTextContent('j');
    // §5.1 row 3: parked ages render bare ("parked 7h"), no "ago".
    expect(view.getByTestId('wf-run-hint')).toHaveTextContent(/parked \d+d · \$0\.00/);
  });

  it('auto-loads only the diff file summaries when entering a gate', async () => {
    setBindingMock('WorkflowGetItem', async () => ({
      item: {
        id: 'run', projectId: 'p', workflowId: 'wf', goal: 'Run', state: 'needs-human', reason: 'gate',
        baseBranch: 'main', worktreePath: '/tmp/run', sortPosition: 0, createdAt: 1,
      },
      phases: [{ phaseId: 'build', attempt: 1, status: 'completed', threadId: 'newest', startedAt: 1, endedAt: 2 }],
      checkPhaseIds: [], outputs: {}, artifacts: [],
      usage: { inputTokens: 0, outputTokens: 0, totalTokens: 0, costUsd: 0 },
    }));
    setBindingMock('GetBranchBaseDiff', async () => [
      'diff --git a/app.go b/app.go',
      '--- a/app.go',
      '+++ b/app.go',
      '@@ -1 +1 @@',
      '-old',
      '+new',
      '',
    ].join('\n'));
    await loadWorkflowCurrentLevel();
    expect(getBindingMock('GetBranchBaseDiff')).toHaveBeenCalledWith('newest', 'main');
    expect(getWorkflowDiffFiles()).toMatchObject([{ path: 'app.go', additions: 1, deletions: 1, lines: [] }]);
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

  it('uses the newest parked phase envelope for the question and suggestions', async () => {
    setBindingMock('WorkflowGetItem', async () => ({
      item: {
        id: 'run', projectId: 'p', workflowId: 'wf', goal: 'Run', state: 'needs-human', reason: 'question',
        digest: JSON.stringify({ whatHappened: 'Asked', whatItNeeds: '1. Stale choice' }),
        baseBranch: 'main', worktreePath: '/tmp/run', sortPosition: 0, createdAt: 1,
      },
      phases: [
        { phaseId: 'build', attempt: 1, status: 'completed', outputEnvelope: JSON.stringify({ status: 'question', question: '1. Older choice', outputs: null, reason: null }), startedAt: 1, endedAt: 2 },
        { phaseId: 'build', attempt: 2, status: 'completed', outputEnvelope: JSON.stringify({ status: 'question', question: '1. Current choice\n2. Another choice', outputs: null, reason: null }), startedAt: 2, endedAt: 3 },
      ],
      outputs: {},
      artifacts: [],
      usage: { inputTokens: 0, outputTokens: 0, totalTokens: 0, costUsd: 0 },
    }));
    await loadWorkflowCurrentLevel();
    const view = render(WorkflowRunDetail, { level });
    expect(await view.findByTestId('wf-question')).toHaveTextContent('Current choice');
    expect(view.getByTestId('wf-suggested-answers')).toHaveTextContent('Current choice');
    expect(view.getByTestId('wf-suggested-answers')).not.toHaveTextContent('Stale choice');
  });

  it('renders the latest failed check evidence and diagnosis attempt', async () => {
    setBindingMock('WorkflowGetItem', async () => ({
      item: {
        id: 'run', projectId: 'p', workflowId: 'wf', goal: 'Run', state: 'failed', reason: 'check-failed-genuine',
        baseBranch: 'main', worktreePath: '/tmp/run', sortPosition: 0, createdAt: 1,
      },
      phases: [
        { phaseId: 'build', attempt: 1, status: 'completed', outputEnvelope: JSON.stringify({ status: 'done', outputs: { passed: false, details: 'Old failure' } }), startedAt: 1, endedAt: 2 },
        { phaseId: 'build', attempt: 3, status: 'completed', outputEnvelope: JSON.stringify({ status: 'done', outputs: { passed: false, details: 'TestParallelDispatch' } }), startedAt: 3, endedAt: 4 },
        { phaseId: 'diagnose', attempt: 3, status: 'completed', outputEnvelope: JSON.stringify({ status: 'done', outputs: { classification: 'genuine', diagnosis: 'claim ordering drops one event' } }), startedAt: 5, endedAt: 6 },
      ],
      checkPhaseIds: ['build'], outputs: {}, artifacts: [],
      usage: { inputTokens: 0, outputTokens: 0, totalTokens: 0, costUsd: 0 },
    }));
    await loadWorkflowCurrentLevel();
    const view = render(WorkflowRunDetail, { level });
    expect(await view.findByTestId('wf-failure-check')).toHaveTextContent('✗ build — TestParallelDispatch ×3 · genuine');
    expect(view.getByTestId('wf-failure-diagnosis')).toHaveTextContent('diagnosis #3: “claim ordering drops one event”');
  });

  it('renders named values before artifacts and opens an artifact through the host opener', async () => {
    setBindingMock('WorkflowGetItem', async () => ({
      item: {
        id: 'run', projectId: 'p', workflowId: 'wf', goal: 'Run', state: 'done', reason: '',
        baseBranch: 'main', worktreePath: '/tmp/run', sortPosition: 0, createdAt: 1,
      },
      phases: [], outputs: { summary: 'All checks passed' },
      artifacts: [{ name: 'report', path: '/tmp/run/report.pdf', size: 12 }],
      checkPhaseIds: [],
      usage: { inputTokens: 0, outputTokens: 0, totalTokens: 0, costUsd: 0 },
    }));
    setBindingMock('OpenInEditor', async () => undefined);
    await loadWorkflowCurrentLevel();
    const view = render(WorkflowRunDetail, { level });
    expect(await view.findByTestId('wf-output-values')).toHaveTextContent('summary');
    expect(view.getByTestId('wf-output-values')).toHaveTextContent('All checks passed');
    await fireEvent.click(view.getByTestId('wf-output-file'));
    expect(getBindingMock('OpenInEditor')).toHaveBeenCalledWith('/tmp/run/report.pdf', 0, 0, '', '');
  });

  it('disables artifact rows remotely with a Local only title', async () => {
    setBindingMock('WorkflowGetItem', async () => ({
      item: {
        id: 'run', projectId: 'p', workflowId: 'wf', goal: 'Run', state: 'done', reason: '',
        baseBranch: 'main', worktreePath: '/tmp/run', sortPosition: 0, createdAt: 1,
      },
      phases: [],
      outputs: {},
      artifacts: [{ name: 'report', path: '/tmp/run/report.md', size: 12 }],
      usage: { inputTokens: 0, outputTokens: 0, totalTokens: 0, costUsd: 0 },
    }));
    await loadWorkflowCurrentLevel();
    (globalThis as { __AO_BOOTSTRAP__?: { remote: boolean } }).__AO_BOOTSTRAP__ = { remote: true };
    __resetRunModeForTest();
    const view = render(WorkflowRunDetail, { level });
    const artifact = await view.findByTestId('wf-output-file') as HTMLButtonElement;
    expect(artifact.disabled).toBe(true);
    expect(artifact.title).toBe('Local only');
  });
});
