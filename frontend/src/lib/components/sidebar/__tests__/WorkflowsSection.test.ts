import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import WorkflowsSection from '../WorkflowsSection.svelte';
import { resetBindingMocks, setBindingMock } from '../../../../test/mocks/bindings-app';
import { resetWorkflowsSidebarForTest, initializeWorkflowsSidebar } from '../../../stores/workflowsSidebar.svelte';
import { getWorkflowCurrentLevel, resetWorkflowsPane } from '../../../stores/workflowsPane.svelte';
import { resetPaneLayoutForTest } from '../../../stores/paneLayout.svelte';
import { resetPanesForTest } from '../../../stores/panes.svelte';

function catalog() {
  return {
    baseBranch: 'main', predictedQueuePosition: 1,
    workflows: [{
      id: 'wf', name: 'Build', scope: 'shared', phaseCount: 2,
      phases: [{ id: 'plan' }, { id: 'build' }], inputs: [], defaultStepMode: false,
      valid: true, allBindingsAvailable: true,
    }],
  };
}

describe('<WorkflowsSection>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWorkflowsSidebarForTest();
    resetWorkflowsPane();
    resetPaneLayoutForTest();
    resetPanesForTest();
    vi.spyOn(Date, 'now').mockReturnValue(120_000);
    setBindingMock('WorkflowListItems', async () => [
      { id: 'needs', projectId: 'p', workflowId: 'wf', goal: 'Needs review', state: 'needs-human', reason: 'gate', sortPosition: 1, createdAt: 1, endedAt: 1 },
      { id: 'running', projectId: 'p', workflowId: 'wf', goal: 'Build it', state: 'running', sortPosition: 2, createdAt: 1, startedAt: 60_000 },
      { id: 'done', projectId: 'p', workflowId: 'wf', goal: 'Dispose it', state: 'done', sortPosition: 3, createdAt: 1 },
      { id: 'resolved', projectId: 'p', workflowId: 'wf', goal: 'Landed', state: 'done', disposition: '{"action":"merged","policy":"manual","at":1}', sortPosition: 4, createdAt: 1 },
    ]);
    setBindingMock('WorkflowListDefinitions', async () => catalog());
    setBindingMock('WorkflowListItemCosts', async () => ({}));
    setBindingMock('WorkflowGetItem', async (itemId: string) => ({
      item: { id: itemId, projectId: 'p', workflowId: 'wf', state: 'running', createdAt: 1 },
      phases: [], artifacts: [], usage: { inputTokens: 0, outputTokens: 0, totalTokens: 0, costUsd: 0 },
    }));
  });

  afterEach(() => {
    vi.restoreAllMocks();
    resetBindingMocks();
  });

  it('rolls up attention, expands ordered live rows, and omits resolved runs', async () => {
    await initializeWorkflowsSidebar();
    const { getByTestId, getAllByTestId, queryByText } = render(WorkflowsSection, { props: { projectId: 'p' } });
    expect(getByTestId('workflows-section-attention').textContent).toContain('1');

    await fireEvent.click(getByTestId('workflows-section-header'));
    const rows = getAllByTestId('workflow-sidebar-run');
    expect(rows.map((row) => row.getAttribute('data-run-id'))).toEqual(['needs', 'running', 'done']);
    expect(rows[0].textContent).toContain('Needs you');
    expect(rows[1].textContent).toContain('running · 1m');
    expect(queryByText('Landed')).toBeNull();
  });

  it('opens a full run stack and highlights the active row', async () => {
    await initializeWorkflowsSidebar();
    const { getByTestId, getAllByTestId } = render(WorkflowsSection, { props: { projectId: 'p' } });
    await fireEvent.click(getByTestId('workflows-section-header'));
    const row = getAllByTestId('workflow-sidebar-run')[0];
    await fireEvent.click(row);
    await waitFor(() => expect(getWorkflowCurrentLevel()).toMatchObject({ kind: 'run', itemId: 'needs' }));
    expect(row.className).toContain('bg-accent/12');
  });

  it('is hidden when the project has no known workflows or runs', async () => {
    resetWorkflowsSidebarForTest();
    setBindingMock('WorkflowListItems', async () => []);
    await initializeWorkflowsSidebar();
    const { queryByTestId } = render(WorkflowsSection, { props: { projectId: 'p' } });
    expect(queryByTestId('workflows-section')).toBeNull();
  });
});
