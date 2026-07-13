import { describe, expect, it, vi } from 'vitest';
import type { WorkItem } from '../types/workflow';
import { dispatchWorkflowAction, workflowActionConfirmationKey, type WorkflowActionBindings } from './workflowActions';

function bindings(): WorkflowActionBindings {
  return {
    answerQuestion: vi.fn(async () => undefined),
    cancelItem: vi.fn(async () => undefined),
    createPR: vi.fn(async () => ({ action: 'pr', prRef: '#12' } as never)),
    discardItem: vi.fn(async () => ({ action: 'discarded' } as never)),
    listItems: vi.fn(async () => [{ id: 'run', projectId: 'p', state: 'queued', sortPosition: 2 } as WorkItem]),
    mergeItem: vi.fn(async () => ({ action: 'merged', base: 'release', mode: 'ff', sha: '1234567890' } as never)),
    reenqueueFailedItem: vi.fn(async () => undefined),
    removeQueuedItem: vi.fn(async () => undefined),
    resolveGate: vi.fn(async () => undefined),
    resumeItem: vi.fn(async () => undefined),
  };
}

describe('workflow action dispatch', () => {
  const item = { id: 'run' } as WorkItem;

  it('uses the verified gate decision strings', async () => {
    const deps = bindings();
    await dispatchWorkflowAction(item, { kind: 'approve' }, 1, deps);
    await dispatchWorkflowAction(item, { kind: 'reject', note: 'revise' }, 1, deps);
    expect(deps.resolveGate).toHaveBeenNthCalledWith(1, 'run', 'approve', '');
    expect(deps.resolveGate).toHaveBeenNthCalledWith(2, 'run', 'reject', 'revise');
  });

  it('re-arms confirmation when item state or reason changes', () => {
    const original = { id: 'run', state: 'running', reason: '' } as WorkItem;
    expect(workflowActionConfirmationKey('cancel', original)).not.toBe(
      workflowActionConfirmationKey('cancel', { ...original, state: 'needs-human', reason: 'stalled' } as WorkItem),
    );
  });

  it('resumes failed items with an empty target phase', async () => {
    const deps = bindings();
    await dispatchWorkflowAction(item, { kind: 'resume' }, 2, deps);
    expect(deps.resumeItem).toHaveBeenCalledWith('run', '');
  });

  it('re-enqueues failed items through the dedicated lifecycle action and reports their rank', async () => {
    const deps = bindings();
    const failed = { id: 'run', projectId: 'p' } as WorkItem;
    const receipt = await dispatchWorkflowAction(failed, { kind: 're-enqueue' }, 2, deps);
    expect(deps.reenqueueFailedItem).toHaveBeenCalledWith('run');
    expect(deps.listItems).toHaveBeenCalledWith('p');
    expect(receipt?.message).toBe('Re-enqueued with the diagnosis as guidance — position 1');
  });

  it('uses the disposition base and human merge mode in merge receipts', async () => {
    const receipt = await dispatchWorkflowAction({ id: 'run', baseBranch: 'main' } as WorkItem, { kind: 'merge' }, 1, bindings());
    expect(receipt?.message).toBe('Merged to release — fast-forward 12345678');
  });

  it('dispatches every remaining action to its exact binding', async () => {
    const cases = [
      { action: { kind: 'answer', answer: 'yes' } as const, binding: 'answerQuestion' as const, args: ['run', 'yes'], receipt: 'answered' },
      { action: { kind: 'merge' } as const, binding: 'mergeItem' as const, args: ['run'], receipt: 'merged' },
      { action: { kind: 'create-pr' } as const, binding: 'createPR' as const, args: ['run'], receipt: 'pr' },
      { action: { kind: 'discard' } as const, binding: 'discardItem' as const, args: ['run'], receipt: 'discarded' },
      { action: { kind: 'cancel' } as const, binding: 'cancelItem' as const, args: ['run'], receipt: null },
      { action: { kind: 'remove' } as const, binding: 'removeQueuedItem' as const, args: ['run'], receipt: 'removed' },
    ];
    for (const testCase of cases) {
      const deps = bindings();
      const result = await dispatchWorkflowAction(item, testCase.action, 3, deps);
      expect(deps[testCase.binding]).toHaveBeenCalledWith(...testCase.args);
      expect(result?.kind ?? null).toBe(testCase.receipt);
      if (result) expect(result.costUsd).toBe(3);
    }
  });
});
