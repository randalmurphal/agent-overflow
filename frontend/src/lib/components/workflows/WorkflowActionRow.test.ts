import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { WorkItem, WorkflowItemDetail } from '../../types/workflow';
import WorkflowActionRow from './WorkflowActionRow.svelte';
import { getWorkflowsActionTargetForTest } from '../../stores/workflowCommands.svelte';
import { recordWorkflowReceipt, resetWorkflowRunsForTest } from '../../stores/workflowRuns.svelte';
import {
  getWorkflowArmedAction,
  getWorkflowsOverlayDialog,
  openWorkflowsOverlay,
  pushWorkflowRunDetail,
  resetWorkflowsOverlayForTest,
} from '../../stores/workflowsOverlay.svelte';
import { cancelWorkflowAutoAdvance } from '../../stores/workflowResolve';
import { resetAppStorageForTest } from '../../stores/appStorage';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { setViewOnlySessionFromBootstrap } from '../../transport/runMode';

function item(over: Partial<WorkItem> = {}): WorkItem {
  return {
    id: 'run-1', projectId: 'p', goal: 'ship it', workflowId: 'port',
    state: 'needs-human', reason: 'gate', createdAt: 1,
    ...over,
  } as WorkItem;
}

function detail(): WorkflowItemDetail {
  return {
    item: { id: 'run-1' },
    checkPhaseIds: [],
    phases: [{ itemId: 'run-1', phaseId: 'port', attempt: 1, status: 'parked', startedAt: 1, threadId: 'thread-port' }],
    units: [], outputs: {}, artifacts: [], usage: { costUsd: 2 },
  } as unknown as WorkflowItemDetail;
}

function mount(over: Partial<WorkItem> = {}, props: Record<string, unknown> = {}) {
  return render(WorkflowActionRow, {
    item: { ...item(), ...over },
    detail: detail(),
    costUsd: 2,
    nextPhaseId: 'docs',
    failedUnitId: '',
    failedUnitThreadId: '',
    onToggleFirstDiff: vi.fn(),
    ...props,
  });
}

function actionButton(view: ReturnType<typeof mount>, id: string): HTMLButtonElement {
  const button = view.container.querySelector(`[data-action-id="${id}"]`);
  if (!button) throw new Error(`no action ${id} on this row`);
  return button as HTMLButtonElement;
}

describe('WorkflowActionRow', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetAppStorageForTest();
    resetWorkflowsOverlayForTest();
    resetWorkflowRunsForTest();
    setViewOnlySessionFromBootstrap(false);
    setBindingMock('WorkflowResolveGate', async () => undefined);
    setBindingMock('WorkflowCancelItem', async () => undefined);
    setBindingMock('WorkflowDropUnit', async () => undefined);
    setBindingMock('WorkflowRetryUnit', async () => undefined);
    setBindingMock('WorkflowRetryFailedUnits', async () => undefined);
    openWorkflowsOverlay();
    pushWorkflowRunDetail('run-1');
  });

  afterEach(() => {
    cancelWorkflowAutoAdvance();
    setViewOnlySessionFromBootstrap(false);
    resetBindingMocks();
  });

  it('renders the gate row, primary first, with its §8 key hint', () => {
    const view = mount();
    const labels = [...view.container.querySelectorAll('[data-testid="workflow-action"]')]
      .map((node) => node.textContent?.trim());
    expect(labels[0]).toContain('Approve → docs');
    expect(labels[0]).toContain('a');
    expect(labels).toHaveLength(2);
  });

  // D32: no row spawns a thread any more. The unit row keeps `Take over unit`,
  // which detaches the unit and opens the thread it is already running in.
  it('offers no thread-spawning action on a parked run', () => {
    for (const over of [
      { reason: 'gate' }, { reason: 'question' }, { reason: 'stuck' },
      { reason: 'paused' }, { reason: 'taken-over' },
      { state: 'failed', reason: 'check-failed-genuine' }, { state: 'done', reason: '' },
    ]) {
      const view = mount(over);
      const ids = [...view.container.querySelectorAll('[data-testid="workflow-action"]')]
        .map((node) => node.getAttribute('data-action-id'));
      expect(ids).not.toContain('take-over');
      expect(ids).not.toContain('open-in-thread');
      view.unmount();
    }
  });

  it('takes over a failed unit through the engine edge before opening its thread', async () => {
    setBindingMock('WorkflowTakeOverUnit', async () => undefined);
    setBindingMock('GetThread', async () => ({ id: 'unit-thread', title: 'unit' }));
    const view = mount({ reason: 'unit-failed' }, { failedUnitId: 'port-3', failedUnitThreadId: 'unit-thread' });
    await fireEvent.click(actionButton(view, 'take-over-unit'));
    await waitFor(() => expect(getBindingMock('WorkflowTakeOverUnit')).toHaveBeenCalledWith('run-1', 'port-3'));
  });

  it('approves through the verified decision string', async () => {
    const view = mount();
    await fireEvent.click(actionButton(view, 'approve'));
    await waitFor(() => expect(getBindingMock('WorkflowResolveGate')).toHaveBeenCalledWith('run-1', 'approve', ''));
  });

  it('opens an inline note for Request changes and commits it as loop feedback', async () => {
    const view = mount();
    await fireEvent.click(actionButton(view, 'request-changes'));
    const note = await view.findByTestId('workflow-note-input');
    await fireEvent.input(note, { target: { value: 'use the shared helper' } });
    await fireEvent.submit(view.getByTestId('workflow-note-form'));
    await waitFor(() => expect(getBindingMock('WorkflowResolveGate'))
      .toHaveBeenCalledWith('run-1', 'reject', 'use the shared helper'));
  });

  it('sends a question answer from the footer input and disables Send until it has text', async () => {
    setBindingMock('WorkflowAnswerQuestion', async () => undefined);
    const view = mount({ reason: 'question' });
    expect(view.getByTestId('workflow-answer-send')).toBeDisabled();
    await fireEvent.input(view.getByTestId('workflow-answer-input'), { target: { value: 'use v2' } });
    await fireEvent.submit(view.getByTestId('workflow-answer-form'));
    await waitFor(() => expect(getBindingMock('WorkflowAnswerQuestion')).toHaveBeenCalledWith('run-1', 'use v2'));
  });

  it('routes discard to the loss preview instead of destroying anything', async () => {
    const view = mount({ state: 'done', reason: '' });
    await fireEvent.click(actionButton(view, 'discard'));
    expect(getWorkflowsOverlayDialog()).toBe('discard');
    expect(getBindingMock('WorkflowDiscardItem')).toBeUndefined();
  });

  it('arms Stop this run before it fires, and the label says so', async () => {
    const view = mount({ state: 'running', reason: '' });
    const stop = actionButton(view, 'cancel');
    await fireEvent.click(stop);
    expect(getWorkflowArmedAction()).toContain('cancel:run-1');
    expect(getBindingMock('WorkflowCancelItem')).not.toHaveBeenCalled();
    await waitFor(() => expect(actionButton(view, 'cancel').textContent).toContain('confirm?'));
    await fireEvent.click(actionButton(view, 'cancel'));
    await waitFor(() => expect(getBindingMock('WorkflowCancelItem')).toHaveBeenCalledWith('run-1'));
  });

  it('arms Drop unit and names the unit it drops', async () => {
    const view = mount({ reason: 'unit-failed' }, { failedUnitId: 'port-3' });
    await fireEvent.click(actionButton(view, 'drop-unit'));
    expect(getBindingMock('WorkflowDropUnit')).not.toHaveBeenCalled();
    await fireEvent.click(actionButton(view, 'drop-unit'));
    await waitFor(() => expect(getBindingMock('WorkflowDropUnit')).toHaveBeenCalledWith('run-1', 'port-3', ''));
  });

  it('refuses a unit action with no failed unit rather than sending an empty id', async () => {
    const view = mount({ reason: 'unit-failed' }, { failedUnitId: '' });
    await fireEvent.click(actionButton(view, 'retry-unit'));
    expect(getBindingMock('WorkflowRetryUnit')).not.toHaveBeenCalled();
    await fireEvent.click(actionButton(view, 'retry-failed-units'));
    expect(getBindingMock('WorkflowRetryFailedUnits')).not.toHaveBeenCalled();
  });

  // The usage-limit recovery: one action for the whole attempt, no unit id, and
  // no confirm to arm — it re-runs work rather than destroying any.
  it('repairs every failed unit in one call, without arming', async () => {
    const view = mount({ reason: 'unit-failed' }, { failedUnitId: 'port-3' });
    await fireEvent.click(actionButton(view, 'retry-failed-units'));
    await waitFor(() => expect(getBindingMock('WorkflowRetryFailedUnits')).toHaveBeenCalledWith('run-1', ''));
    expect(getBindingMock('WorkflowRetryUnit')).not.toHaveBeenCalled();
  });

  describe('§8 key target', () => {
    it('binds `u` to the whole-attempt repair on the unit-failed row', async () => {
      mount({ reason: 'unit-failed' }, { failedUnitId: 'port-3' });
      getWorkflowsActionTargetForTest()?.action('u');
      await waitFor(() => expect(getBindingMock('WorkflowRetryFailedUnits')).toHaveBeenCalledWith('run-1', ''));
    });

    it('maps a / r / t onto this row and Enter onto the first diff file', async () => {
      const onToggleFirstDiff = vi.fn();
      mount({}, { onToggleFirstDiff });
      const target = getWorkflowsActionTargetForTest();
      expect(target).not.toBeNull();

      target?.action('a');
      await waitFor(() => expect(getBindingMock('WorkflowResolveGate')).toHaveBeenCalledWith('run-1', 'approve', ''));
      target?.enter();
      expect(onToggleFirstDiff).toHaveBeenCalledTimes(1);
    });

    it('focuses the answer input on `a` for a question — there is nothing to commit yet', async () => {
      const view = mount({ reason: 'question' });
      getWorkflowsActionTargetForTest()?.action('a');
      await waitFor(() => expect(document.activeElement).toBe(view.getByTestId('workflow-answer-input')));
    });

    it('commits the open note on Enter', async () => {
      const view = mount();
      await fireEvent.click(actionButton(view, 'request-changes'));
      await view.findByTestId('workflow-note-input');
      getWorkflowsActionTargetForTest()?.enter();
      await waitFor(() => expect(getBindingMock('WorkflowResolveGate')).toHaveBeenCalledWith('run-1', 'reject', ''));
    });

    it('ignores a key on a row the human has already resolved', () => {
      recordWorkflowReceipt({ itemId: 'run-1', kind: 'approved', message: 'Approved', costUsd: 1 });
      mount();
      getWorkflowsActionTargetForTest()?.action('a');
      expect(getBindingMock('WorkflowResolveGate')).not.toHaveBeenCalled();
    });
  });

  it('shows the session receipt with a way back instead of the action row', () => {
    recordWorkflowReceipt({ itemId: 'run-1', kind: 'approved', message: 'Approved — routing to docs', costUsd: 1 });
    const view = mount();
    expect(view.getByTestId('workflow-resolved-receipt')).toHaveTextContent('Approved — routing to docs');
    expect(view.getByTestId('workflow-receipt-back')).toBeInTheDocument();
    expect(view.container.querySelector('[data-testid="workflow-action"]')).toBeNull();
  });

  describe('remote posture (§10)', () => {
    it('disables every action with a Local only tooltip', () => {
      setViewOnlySessionFromBootstrap(true);
      const view = mount();
      const buttons = [...view.container.querySelectorAll('[data-testid="workflow-action"]')] as HTMLButtonElement[];
      expect(buttons.length).toBeGreaterThan(0);
      for (const button of buttons) {
        expect(button.disabled).toBe(true);
        expect(button.title).toBe('Local only');
      }
    });

    it('ignores the §8 keys too — the guard is not just visual', () => {
      setViewOnlySessionFromBootstrap(true);
      mount();
      getWorkflowsActionTargetForTest()?.action('a');
      expect(getBindingMock('WorkflowResolveGate')).not.toHaveBeenCalled();
    });

    it('refuses the whole-attempt repair in a view-only session, by click and by key', async () => {
      setViewOnlySessionFromBootstrap(true);
      const view = mount({ reason: 'unit-failed' }, { failedUnitId: 'port-3' });
      await fireEvent.click(actionButton(view, 'retry-failed-units'));
      getWorkflowsActionTargetForTest()?.action('u');
      expect(getBindingMock('WorkflowRetryFailedUnits')).not.toHaveBeenCalled();
    });
  });
});
