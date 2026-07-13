import { render } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Project } from '../../types/models';
import type { WorkItem } from '../../types/workflow';
import { addProjectLocal, resetProjectsForTest } from '../../stores/projects.svelte';
import {
  activateWorkflowsPane,
  loadWorkflowOverview,
  resetWorkflowsPane,
} from '../../stores/workflowsPane.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import WorkflowDetail from './WorkflowDetail.svelte';

const project: Project = {
  id: 'p', name: 'Project', path: '/tmp/p', sortPosition: 0,
  createdAt: 1, updatedAt: 1, archived: false,
};

function historyItem(id: string, disposition: unknown): WorkItem {
  return {
    id, projectId: 'p', goal: id, workflowId: 'wf', workflowScope: 'shared',
    state: 'done', reason: '', sortPosition: 0, stepMode: false, source: 'manual',
    disposition, createdAt: Date.now() - 7_200_000, endedAt: Date.now() - 3_600_000,
  } as WorkItem;
}

describe('WorkflowDetail history receipts', () => {
  beforeEach(async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-07-12T12:00:00Z'));
    resetWorkflowsPane();
    resetProjectsForTest();
    addProjectLocal(project);
    setBindingMock('WorkflowListItems', async () => [
      historyItem('awaiting', ''),
      historyItem('merged', { action: 'merged', mode: 'ff', sha: 'abc', policy: 'manual', at: Date.now() }),
      historyItem('pr', JSON.stringify({ action: 'pr', prRef: '#42', policy: 'manual', at: Date.now() })),
      historyItem('discarded', { action: 'discarded', policy: 'manual', at: Date.now() }),
      { ...historyItem('failed-discarded', { action: 'discarded', policy: 'manual', at: Date.now() }), state: 'failed', reason: 'check-failed-genuine' } as WorkItem,
    ]);
    setBindingMock('WorkflowListItemCosts', async () => ({ awaiting: 1, merged: 2, pr: 3, discarded: 4, 'failed-discarded': 5 }));
    setBindingMock('WorkflowListDefinitions', async () => ({
      baseBranch: 'main', predictedQueuePosition: 1,
      workflows: [{ id: 'wf', name: 'Workflow', scope: 'shared', phaseCount: 0, phases: [], inputs: [], defaultStepMode: false, valid: true, allBindingsAvailable: true }],
    }));
    activateWorkflowsPane();
    await loadWorkflowOverview();
  });

  afterEach(() => {
    resetBindingMocks();
    vi.useRealTimers();
  });

  it('keeps awaiting-disposition runs live and renders disposed history receipts', () => {
    const view = render(WorkflowDetail, {
      level: { kind: 'workflow', projectId: 'p', workflowId: 'wf', label: 'Workflow' },
    });
    const rows = view.getAllByTestId('wf-history-row').map((row) => row.textContent ?? '');
    expect(rows).toEqual(expect.arrayContaining([
      expect.stringContaining('merged · 1h · $2.00'),
      expect.stringContaining('PR #42 · 1h · $3.00'),
      expect.stringContaining('discarded · 1h · $4.00'),
    ]));
    expect(rows.some((row) => row.includes('awaiting'))).toBe(false);
    expect(view.getByTestId('wf-live-runs')).toHaveTextContent('awaiting');
    expect(view.getByTestId('wf-live-runs')).toHaveTextContent('Finished 1h ago · $1.00');
  });

  it('moves a discarded failed run to history instead of keeping it parked', () => {
    const view = render(WorkflowDetail, {
      level: { kind: 'workflow', projectId: 'p', workflowId: 'wf', label: 'Workflow' },
    });
    const rows = view.getAllByTestId('wf-history-row').map((row) => row.textContent ?? '');
    expect(rows.some((row) => row.includes('failed-discarded') && row.includes('discarded · 1h · $5.00'))).toBe(true);
    expect(view.getByTestId('wf-live-runs')).not.toHaveTextContent('failed-discarded');
  });
});
