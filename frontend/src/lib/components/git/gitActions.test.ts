// Git action coverage:
//   - `primaryActionFor` is pure (decision table). Test every branch.
//   - `runPushAction` / `runPullAction` / `runCreatePRAction` handle
//     result.error vs thrown errors differently — conflating them
//     flips success toasts on push failures. Assert both paths.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import {
  primaryActionFor,
  runPushAction,
  runPullAction,
  runCreatePRAction,
  runRemoveWorktreeAction,
  type GitActionCtx,
  type RemoveWorktreeCtx,
} from './gitActions';
import type { GitStatus, WorkspaceRef } from '../../types/git';
import {
  resetBindingMocks,
  setBindingMock,
} from '../../../test/mocks/bindings-app';

function status(overrides: Partial<GitStatus> = {}): GitStatus {
  return {
    isRepo: true,
    branch: 'main',
    isDefaultBranch: true,
    hasChanges: false,
    insertions: 0,
    deletions: 0,
    fileCount: 0,
    hasUpstream: true,
    aheadCount: 0,
    behindCount: 0,
    hasOriginRemote: true,
    ...overrides,
  };
}

const WS: WorkspaceRef = { projectId: 'project-1', workspacePath: '/workspace' };

function ctx(overrides: Partial<GitActionCtx> = {}): GitActionCtx {
  return {
    workspace: WS,
    reportError: vi.fn(),
    refreshStatus: vi.fn().mockResolvedValue(undefined),
    ...overrides,
  };
}

/** The worktree-removal action names the worktree it is removing on top of
 *  the checkout it is removing it FROM. */
function removeCtx(overrides: Partial<RemoveWorktreeCtx> = {}): RemoveWorktreeCtx {
  return { ...ctx(), worktreePath: '/workspace/.worktrees/feature', ...overrides };
}

describe('primaryActionFor', () => {
  it('returns a disabled Commit label when status is null (loading)', () => {
    const out = primaryActionFor(null);
    expect(out).toEqual({
      label: 'Commit',
      action: 'commit',
      disabled: true,
      tooltip: 'Loading...',
    });
  });

  it('surfaces Commit when there are uncommitted changes', () => {
    const out = primaryActionFor(status({ hasChanges: true }));
    expect(out.action).toBe('commit');
    expect(out.disabled).toBe(false);
    expect(out.tooltip).toBe('Stage and commit changes');
  });

  it('surfaces Push when no changes but ahead of upstream (singular)', () => {
    const out = primaryActionFor(status({ aheadCount: 1 }));
    expect(out.action).toBe('push');
    expect(out.tooltip).toBe('Push 1 commit');
  });

  it('surfaces Push with plural tooltip when ahead > 1', () => {
    const out = primaryActionFor(status({ aheadCount: 3 }));
    expect(out.tooltip).toBe('Push 3 commits');
  });

  it('surfaces Pull when no changes, not ahead, but behind upstream (singular)', () => {
    const out = primaryActionFor(status({ behindCount: 1 }));
    expect(out.action).toBe('pull');
    expect(out.tooltip).toBe('Pull 1 commit');
  });

  it('surfaces Pull with plural tooltip when behind > 1', () => {
    const out = primaryActionFor(status({ behindCount: 2 }));
    expect(out.tooltip).toBe('Pull 2 commits');
  });

  it('priority: hasChanges wins over ahead/behind', () => {
    const out = primaryActionFor(status({ hasChanges: true, aheadCount: 3, behindCount: 2 }));
    expect(out.action).toBe('commit');
  });

  it('priority: ahead wins over behind when no changes', () => {
    const out = primaryActionFor(status({ aheadCount: 1, behindCount: 1 }));
    expect(out.action).toBe('push');
  });

  it('falls back to a disabled Commit "no changes" when idle', () => {
    const out = primaryActionFor(status({}));
    expect(out).toEqual({
      label: 'Commit',
      action: 'commit',
      disabled: true,
      tooltip: 'No changes to commit',
    });
  });
});

describe('runPushAction', () => {
  beforeEach(() => resetBindingMocks());

  it('reports result.error without throwing and without refreshing status', async () => {
    setBindingMock('GitPush', async () => ({ error: 'Repo has diverged', commitSha: '' }));
    const c = ctx();
    await runPushAction(c);
    expect(c.reportError).toHaveBeenCalledWith('Push failed: Repo has diverged');
    expect(c.refreshStatus).not.toHaveBeenCalled();
  });

  it('surfaces a thrown error via errString', async () => {
    setBindingMock('GitPush', async () => {
      throw new Error('network down');
    });
    const c = ctx();
    await runPushAction(c);
    expect(c.reportError).toHaveBeenCalledWith('Push failed: network down');
  });

  it('refreshes status on success', async () => {
    setBindingMock('GitPush', async () => ({}));
    const c = ctx();
    await runPushAction(c);
    expect(c.reportError).not.toHaveBeenCalled();
    expect(c.refreshStatus).toHaveBeenCalledTimes(1);
  });
});

describe('runPullAction', () => {
  beforeEach(() => resetBindingMocks());

  it('reports result.error without throwing', async () => {
    setBindingMock('GitPull', async () => ({ error: 'conflict' }));
    const c = ctx();
    await runPullAction(c);
    expect(c.reportError).toHaveBeenCalledWith('Pull failed: conflict');
    expect(c.refreshStatus).not.toHaveBeenCalled();
  });

  it('surfaces a thrown error via errString', async () => {
    setBindingMock('GitPull', async () => {
      throw new Error('offline');
    });
    const c = ctx();
    await runPullAction(c);
    expect(c.reportError).toHaveBeenCalledWith('Pull failed: offline');
  });
});

describe('runCreatePRAction', () => {
  beforeEach(() => resetBindingMocks());

  it('reports result.error on PR creation failure', async () => {
    setBindingMock('GitCreatePR', async () => ({ error: 'already open' }));
    const c = ctx();
    await runCreatePRAction(c);
    expect(c.reportError).toHaveBeenCalledWith('Create PR failed: already open');
  });

  it('surfaces a thrown error via errString', async () => {
    setBindingMock('GitCreatePR', async () => {
      throw new Error('no auth');
    });
    const c = ctx();
    await runCreatePRAction(c);
    expect(c.reportError).toHaveBeenCalledWith('Create PR failed: no auth');
  });
});

describe('runRemoveWorktreeAction', () => {
  beforeEach(() => resetBindingMocks());

  it('removes the named worktree from the pane\'s checkout and refreshes', async () => {
    const remove = setBindingMock('RemoveOtherWorktree', async () => ({
      workspacePath: '/workspace',
      worktreePath: '',
      branch: 'main',
    }));
    const c = removeCtx();
    await runRemoveWorktreeAction(c);
    // Thread rows reattach through ThreadUpdated; what this action owns is
    // the RPC's subject — the checkout, plus the worktree being removed.
    expect(remove).toHaveBeenCalledWith(WS, '/workspace/.worktrees/feature', false);
    expect(c.refreshStatus).toHaveBeenCalledTimes(1);
    expect(c.reportError).not.toHaveBeenCalled();
  });

  it('surfaces a thrown error via errString and does not refresh', async () => {
    setBindingMock('RemoveOtherWorktree', async () => {
      throw new Error('worktree is dirty');
    });
    const c = removeCtx();
    await runRemoveWorktreeAction(c);
    expect(c.reportError).toHaveBeenCalledWith('Remove worktree failed: worktree is dirty');
    expect(c.refreshStatus).not.toHaveBeenCalled();
  });
});
