import { describe, expect, it } from 'vitest';
import { cleanupSummary, retainedInPreview, retainedNotice } from './projectCleanup';
import type { ProjectCleanupWorktree, RetainedWorktree } from '../types/workflow';

function worktree(overrides: Partial<ProjectCleanupWorktree> = {}): ProjectCleanupWorktree {
  return {
    path: '/tmp/wt',
    branch: 'workflow-run-1',
    dirtyFileCount: 0,
    retained: false,
    ...overrides,
  };
}

describe('cleanupSummary', () => {
  it('is empty when the project owns no workflow work', () => {
    expect(cleanupSummary(0, 0, 0)).toBe('');
  });

  it('counts runs and automations separately', () => {
    expect(cleanupSummary(1, 0, 0)).toBe('1 workflow run will be deleted with it.');
    expect(cleanupSummary(2, 0, 3)).toBe(
      '2 workflow runs and 3 automations will be deleted with it.',
    );
    expect(cleanupSummary(0, 0, 1)).toBe('1 automation will be deleted with it.');
  });

  it('calls out the runs that are stopped rather than merely deleted', () => {
    expect(cleanupSummary(3, 1, 0)).toBe(
      '3 workflow runs will be deleted with it, including 1 still working — it is stopped first.',
    );
    expect(cleanupSummary(3, 2, 0)).toContain(
      'including 2 still working — they are stopped first.',
    );
  });
});

describe('retainedInPreview', () => {
  it('keeps only the checkouts the cleanup will leave behind', () => {
    const rows = [
      worktree({ path: '/tmp/clean' }),
      worktree({ path: '/tmp/dirty', retained: true, dirtyFileCount: 2, reason: '2 uncommitted files' }),
    ];
    expect(retainedInPreview(rows).map((row) => row.path)).toEqual(['/tmp/dirty']);
  });
});

describe('retainedNotice', () => {
  it('says nothing when the cleanup removed everything', () => {
    expect(retainedNotice([])).toBe('');
  });

  it('names each path with the reason it survived', () => {
    const retained: RetainedWorktree[] = [
      { path: '/tmp/a', branch: 'workflow-a', reason: '2 uncommitted or untracked files' },
      { path: '/tmp/b', branch: 'workflow-b', reason: '1 uncommitted or untracked file' },
    ];
    expect(retainedNotice(retained)).toBe(
      '2 checkouts left in place: /tmp/a (2 uncommitted or untracked files); ' +
        '/tmp/b (1 uncommitted or untracked file)',
    );
  });
});
