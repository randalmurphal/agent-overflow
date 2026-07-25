import { describe, expect, it } from 'vitest';
import { worktreeLossSummary } from './workflowLoss';
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
