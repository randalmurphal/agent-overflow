import { describe, expect, it, vi } from 'vitest';
import type { WorkItem } from '../types/workflow';
import { dispatchWorkflowAction, workflowActionConfirmationKey, type WorkflowActionBindings } from './workflowActions';

function bindings(): WorkflowActionBindings {
  return {
    answerQuestion: vi.fn(async () => undefined),
    cancelItem: vi.fn(async () => undefined),
    completeTakeover: vi.fn(async () => undefined),
    createPR: vi.fn(async () => ({ prRef: '#12' })),
    discardItem: vi.fn(async () => ({ discarded: { removedWorktrees: ['a', 'b', 'c'] } })),
    dropUnit: vi.fn(async () => undefined),
    mergeItem: vi.fn(async () => ({ base: 'release', mode: 'ff', sha: '1234567890' })),
    pauseItem: vi.fn(async () => undefined),
    requestSoftStop: vi.fn(async () => undefined),
    resolveGate: vi.fn(async () => undefined),
    resumeItem: vi.fn(async () => undefined),
    rerunItem: vi.fn(async () => undefined),
    retryUnit: vi.fn(async () => undefined),
    retryFailedUnits: vi.fn(async () => undefined),
  };
}

describe('workflow action dispatch', () => {
  const item = { id: 'run' } as WorkItem;

  it('uses the verified gate decision strings', async () => {
    const deps = bindings();
    await dispatchWorkflowAction(item, { kind: 'approve' }, 1, deps);
    await dispatchWorkflowAction(item, { kind: 'request-changes', note: 'revise' }, 1, deps);
    expect(deps.resolveGate).toHaveBeenNthCalledWith(1, 'run', 'approve', '');
    expect(deps.resolveGate).toHaveBeenNthCalledWith(2, 'run', 'reject', 'revise');
  });

  it('re-arms confirmation when item state or reason changes', () => {
    const original = { id: 'run', state: 'running', reason: '' } as WorkItem;
    expect(workflowActionConfirmationKey('cancel', original)).not.toBe(
      workflowActionConfirmationKey('cancel', { ...original, state: 'needs-human', reason: 'stalled' } as WorkItem),
    );
  });

  it('resumes parked items with an empty target phase', async () => {
    const deps = bindings();
    await dispatchWorkflowAction(item, { kind: 'resume' }, 2, deps);
    expect(deps.resumeItem).toHaveBeenCalledWith('run', '', false);
  });

  it('distinguishes a guided rerun from a diagnosis-seeded one', async () => {
    const deps = bindings();
    const blind = await dispatchWorkflowAction(item, { kind: 'rerun', guidance: '' }, 2, deps);
    const guided = await dispatchWorkflowAction(item, { kind: 'rerun', guidance: 'skip the cache' }, 2, deps);
    expect(deps.rerunItem).toHaveBeenNthCalledWith(1, 'run', '', false);
    expect(deps.rerunItem).toHaveBeenNthCalledWith(2, 'run', 'skip the cache', false);
    expect(blind?.message).toBe('Rerunning — the diagnosis seeds the new attempt');
    expect(guided?.message).toBe('Rerunning — your guidance seeds the new attempt');
  });

  it('uses the disposition base and human merge mode in merge receipts', async () => {
    const receipt = await dispatchWorkflowAction({ id: 'run', baseBranch: 'main' } as WorkItem, { kind: 'merge' }, 1, bindings());
    expect(receipt?.message).toBe('Merged to release — fast-forward 12345678 · worktree cleaned');
  });

  it('omits worktree cleanup from a merge receipt when cleanup failed', async () => {
    const deps = bindings();
    deps.mergeItem = vi.fn(async () => ({ base: 'main', mode: 'ff', sha: 'abc', cleanupFailed: true }));
    const receipt = await dispatchWorkflowAction({ id: 'run' } as WorkItem, { kind: 'merge' }, 1, deps);
    expect(receipt?.message).toBe('Merged to main — fast-forward abc');
  });

  it('accounts for the worktrees a discard actually removed', async () => {
    const many = await dispatchWorkflowAction(item, { kind: 'discard' }, 1, bindings());
    expect(many?.message).toBe('Discarded — 3 worktrees removed, record kept');

    const oneDeps = bindings();
    oneDeps.discardItem = vi.fn(async () => ({ discarded: { removedWorktrees: ['only'] } }));
    expect((await dispatchWorkflowAction(item, { kind: 'discard' }, 1, oneDeps))?.message)
      .toBe('Discarded — 1 worktree removed, record kept');

    const noneDeps = bindings();
    noneDeps.discardItem = vi.fn(async () => ({ discarded: null }));
    expect((await dispatchWorkflowAction(item, { kind: 'discard' }, 1, noneDeps))?.message)
      .toBe('Discarded — record kept');
  });

  it('names the unit in a fan-out receipt', async () => {
    const deps = bindings();
    const retried = await dispatchWorkflowAction(item, { kind: 'retry-unit', unitId: 'port-3', note: 'flaky' }, 1, deps);
    const dropped = await dispatchWorkflowAction(item, { kind: 'drop-unit', unitId: 'port-3', note: '' }, 1, deps);
    expect(deps.retryUnit).toHaveBeenCalledWith('run', 'port-3', 'flaky');
    expect(deps.dropUnit).toHaveBeenCalledWith('run', 'port-3', '');
    expect(retried?.message).toContain('port-3');
    expect(dropped?.message).toBe('Unit dropped — the join proceeds without port-3');
  });

  // The whole-attempt repair names no unit — it has no single one to name — so
  // its receipt says what survives instead.
  it('repairs every failed unit with one call and no unit id', async () => {
    const deps = bindings();
    const receipt = await dispatchWorkflowAction(item, { kind: 'retry-failed-units', note: 'limit reset' }, 3, deps);
    expect(deps.retryFailedUnits).toHaveBeenCalledWith('run', 'limit reset');
    expect(deps.retryUnit).not.toHaveBeenCalled();
    expect(receipt).toMatchObject({
      itemId: 'run',
      kind: 'restarted',
      message: 'Retrying every failed unit — finished units keep their results',
      costUsd: 3,
    });
  });

  it('returns no receipt for the actions that do not resolve the run', async () => {
    const deps = bindings();
    expect(await dispatchWorkflowAction(item, { kind: 'pause' }, 1, deps)).toBeNull();
    expect(await dispatchWorkflowAction(item, { kind: 'cancel' }, 1, deps)).toBeNull();
    expect(deps.pauseItem).toHaveBeenCalledWith('run');
    expect(deps.cancelItem).toHaveBeenCalledWith('run');
  });

  it('carries both directions of the stop request to the one binding', async () => {
    const deps = bindings();
    expect(await dispatchWorkflowAction(item, { kind: 'soft-stop', armed: true }, 1, deps)).toBeNull();
    expect(deps.requestSoftStop).toHaveBeenCalledWith('run', true);
    expect(await dispatchWorkflowAction(item, { kind: 'soft-stop', armed: false }, 1, deps)).toBeNull();
    expect(deps.requestSoftStop).toHaveBeenLastCalledWith('run', false);
  });

  it('dispatches every remaining action to its exact binding', async () => {
    const cases = [
      { action: { kind: 'answer', answer: 'yes' } as const, binding: 'answerQuestion' as const, args: ['run', 'yes'], receipt: 'answered' },
      { action: { kind: 'complete-takeover' } as const, binding: 'completeTakeover' as const, args: ['run'], receipt: 'handed-off' },
      { action: { kind: 'merge' } as const, binding: 'mergeItem' as const, args: ['run'], receipt: 'merged' },
      { action: { kind: 'create-pr' } as const, binding: 'createPR' as const, args: ['run'], receipt: 'pr' },
      { action: { kind: 'discard' } as const, binding: 'discardItem' as const, args: ['run'], receipt: 'discarded' },
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
