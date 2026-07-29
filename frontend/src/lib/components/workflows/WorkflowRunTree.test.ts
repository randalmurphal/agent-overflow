import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { WorkflowItemDetail } from '../../types/workflow';
import WorkflowRunTree from './WorkflowRunTree.svelte';
import { resetWorkflowRunsForTest } from '../../stores/workflowRuns.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

function detail(over: Partial<WorkflowItemDetail> = {}): WorkflowItemDetail {
  return {
    item: { id: 'run-1' }, checkPhaseIds: [], phases: [], units: [], children: [],
    outputs: {}, artifacts: [], usage: { costUsd: 0 },
    ...over,
  } as unknown as WorkflowItemDetail;
}

const parent = detail({
  phases: [
    { itemId: 'run-1', phaseId: 'plan', attempt: 1, status: 'completed', startedAt: 100, endedAt: 160, threadId: 'thread-plan' },
    { itemId: 'run-1', phaseId: 'call', attempt: 1, status: 'running', startedAt: 200 },
  ],
  units: [
    { itemId: 'run-1', phaseId: 'plan', attempt: 1, unitId: 'plan-a', unitIndex: 0, kind: 'unit', status: 'done', unitAttempt: 1, threadId: 'thread-unit' },
    { itemId: 'run-1', phaseId: 'plan', attempt: 1, unitId: 'plan-join', unitIndex: 9, kind: 'join', status: 'done', unitAttempt: 1 },
  ],
  children: [
    { itemId: 'child-1', workflowId: 'audit', state: 'running', parentPhaseId: 'call', parentAttempt: 1, callDepth: 1, currentPhaseOrdinal: 1, phaseCount: 2 },
  ],
} as Partial<WorkflowItemDetail>);

const child = detail({
  item: { id: 'child-1' },
  phases: [{ itemId: 'child-1', phaseId: 'audit', attempt: 1, status: 'running', startedAt: 300, threadId: 'thread-audit' }],
} as Partial<WorkflowItemDetail>);

describe('WorkflowRunTree', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWorkflowRunsForTest();
    setBindingMock('WorkflowGetItem', async () => child);
  });

  afterEach(() => {
    resetBindingMocks();
    resetWorkflowRunsForTest();
  });

  it('renders phase rows with their units and opens any row that has a thread', async () => {
    const onOpenThread = vi.fn();
    const view = render(WorkflowRunTree, { detail: parent, onOpenThread });

    expect(view.getAllByTestId('workflow-phase-row').map((node) => node.textContent?.trim()))
      .toEqual([expect.stringContaining('plan'), expect.stringContaining('call')]);
    expect(view.getAllByTestId('workflow-unit-row').map((node) => node.getAttribute('data-unit-id')))
      .toEqual(['plan-a', 'plan-join']);

    await fireEvent.click(view.getAllByTestId('workflow-phase-row')[0]);
    expect(onOpenThread).toHaveBeenCalledWith('thread-plan');
    await fireEvent.click(view.getAllByTestId('workflow-unit-row')[0]);
    expect(onOpenThread).toHaveBeenCalledWith('thread-unit');
  });

  it('leaves a thread-less row inert rather than opening nothing', async () => {
    const onOpenThread = vi.fn();
    const view = render(WorkflowRunTree, { detail: parent, onOpenThread });
    const callPhase = view.getAllByTestId('workflow-phase-row')[1] as HTMLButtonElement;
    expect(callPhase.disabled).toBe(true);
    await fireEvent.click(callPhase);
    expect(onOpenThread).not.toHaveBeenCalled();
  });

  it('expands a call phase into its child run, recursively, loading on demand', async () => {
    const view = render(WorkflowRunTree, { detail: parent, onOpenThread: vi.fn() });
    expect(view.getAllByTestId('workflow-run-tree')).toHaveLength(1);

    const toggle = view.getByTestId('workflow-child-toggle');
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await fireEvent.click(toggle);

    await waitFor(() => expect(view.getAllByTestId('workflow-run-tree')).toHaveLength(2));
    const nested = view.getAllByTestId('workflow-run-tree')[1];
    expect(nested).toHaveAttribute('data-depth', '1');
    expect(nested).toHaveTextContent('audit');

    await fireEvent.click(toggle);
    await waitFor(() => expect(view.getAllByTestId('workflow-run-tree')).toHaveLength(1));
  });

  it('expands a call-bound unit into its own child run, nested under that unit', async () => {
    const campaign = detail({
      phases: [{ itemId: 'run-1', phaseId: 'wave', attempt: 1, status: 'running', startedAt: 100 }],
      units: [
        { itemId: 'run-1', phaseId: 'wave', attempt: 1, unitId: 'wave-a', unitIndex: 0, kind: 'unit', status: 'running', unitAttempt: 1 },
        { itemId: 'run-1', phaseId: 'wave', attempt: 1, unitId: 'wave-join', unitIndex: 9, kind: 'join', status: 'pending', unitAttempt: 1 },
      ],
      children: [
        {
          itemId: 'child-1', workflowId: 'audit', state: 'running', parentPhaseId: 'wave',
          parentUnitId: 'wave-a', parentAttempt: 1, callDepth: 1, currentPhaseOrdinal: 1, phaseCount: 2,
        },
      ],
    } as Partial<WorkflowItemDetail>);
    const view = render(WorkflowRunTree, { detail: campaign, onOpenThread: vi.fn() });

    // The child row lives inside the unit list, not as a sibling of the phase.
    const childRun = view.getByTestId('workflow-child-run');
    expect(childRun.closest('[data-testid="workflow-unit-list"]')).not.toBeNull();
    expect(childRun.closest('li')?.querySelector('[data-unit-id]')?.getAttribute('data-unit-id'))
      .toBe('wave-a');

    await fireEvent.click(view.getByTestId('workflow-child-toggle'));
    await waitFor(() => expect(view.getAllByTestId('workflow-run-tree')).toHaveLength(2));
    expect(view.getAllByTestId('workflow-run-tree')[1]).toHaveTextContent('audit');
  });

  it('highlights the unit a unit-failed park is about', () => {
    const failing = detail({
      phases: [{ itemId: 'run-1', phaseId: 'port', attempt: 1, status: 'parked', startedAt: 100 }],
      units: [
        { itemId: 'run-1', phaseId: 'port', attempt: 1, unitId: 'port-a', unitIndex: 0, kind: 'unit', status: 'done', unitAttempt: 1 },
        { itemId: 'run-1', phaseId: 'port', attempt: 1, unitId: 'port-b', unitIndex: 1, kind: 'unit', status: 'failed', unitAttempt: 1 },
      ],
    } as Partial<WorkflowItemDetail>);
    const view = render(WorkflowRunTree, { detail: failing, highlightUnitId: 'port-b', onOpenThread: vi.fn() });
    const rows = view.getAllByTestId('workflow-unit-row');
    expect(rows[0].className).not.toContain('bg-error/10');
    expect(rows[1].className).toContain('bg-error/10');
  });
});
