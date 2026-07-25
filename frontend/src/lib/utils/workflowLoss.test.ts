import { describe, expect, it } from 'vitest';
import { runLossSummary, worktreeLossSummary } from './workflowLoss';
import type { WorkflowDiscardWorktree } from '../types/workflow';

function worktree(overrides: Partial<WorkflowDiscardWorktree> = {}): WorkflowDiscardWorktree {
  return {
    itemId: 'run-1',
    path: '/tmp/wt',
    branch: 'workflow-run-1',
    base: 'main',
    present: true,
    registered: true,
    dirtyFiles: [],
    dirtyFileCount: 0,
    unmergedCommits: [],
    unmergedCommitCount: 0,
    ...overrides,
  };
}

describe('worktreeLossSummary', () => {
  it('names the branch and counts what only lives in the checkout', () => {
    expect(worktreeLossSummary(worktree({ dirtyFileCount: 1, unmergedCommitCount: 3 }))).toBe(
      'workflow-run-1 · 1 dirty file · 3 unmerged commits',
    );
  });

  it('says when the checkout is already gone or was never ours to remove', () => {
    expect(worktreeLossSummary(worktree({ present: false }))).toContain('checkout already gone');
    expect(worktreeLossSummary(worktree({ registered: false }))).toContain('reported, not removed');
  });

  it('surfaces an inspection failure instead of describing the row as clean', () => {
    expect(worktreeLossSummary(worktree({ error: 'inspect working tree: boom' }))).toBe(
      'inspect working tree: boom',
    );
  });

  it('does not claim a branch it was not given', () => {
    expect(worktreeLossSummary(worktree({ branch: '' }))).toContain('no branch');
  });
});

describe('runLossSummary', () => {
  it('is empty when there is nothing to lose', () => {
    expect(runLossSummary(0, 0, 0)).toBe('');
  });

  it('counts runs and automations separately', () => {
    expect(runLossSummary(1, 0, 0)).toBe('1 workflow run will be deleted.');
    expect(runLossSummary(2, 0, 3)).toBe('2 workflow runs and 3 automations will be deleted.');
    expect(runLossSummary(0, 0, 1)).toBe('1 automation will be deleted.');
  });

  it('calls out the runs that are stopped rather than merely thrown away', () => {
    expect(runLossSummary(3, 1, 0)).toBe(
      '3 workflow runs will be deleted, including 1 still working — it is stopped first.',
    );
    expect(runLossSummary(3, 2, 0)).toContain('including 2 still working — they are stopped first.');
  });
});
