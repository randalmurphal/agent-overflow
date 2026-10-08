// Git action coverage:
//   - `primaryActionFor` and `menuPushEnabled` are pure (decision tables).
//     Test every branch.
//   - `runPushAction` / `runPullAction` handle result.error vs thrown
//     errors differently — conflating them flips success toasts on push
//     failures. Assert both paths.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import {
  menuPushEnabled,
  primaryActionFor,
  runPushAction,
  runPullAction,
  runRemoveWorktreeAction,
  type GitActionCtx,
  type RemoveWorktreeCtx,
} from './gitActions';
import type { GitStatus, WorkspaceRef } from '../../types/git';
import {
  resetBindingMocks,
  setBindingMock,
} from '../../../test/mocks/bindings-app';
import { buildPane, makeThread } from '../../../test/helpers/chat';
import { resetPanesForTest } from '../../stores/panes.svelte';

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
    const failure = new Error('network down');
    setBindingMock('GitPush', async () => {
      throw failure;
    });
    const c = ctx();
    await runPushAction(c);
    expect(c.reportError).toHaveBeenCalledWith('Push failed: network down', failure);
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
    const failure = new Error('offline');
    setBindingMock('GitPull', async () => {
      throw failure;
    });
    const c = ctx();
    await runPullAction(c);
    expect(c.reportError).toHaveBeenCalledWith('Pull failed: offline', failure);
  });
});

describe('menuPushEnabled', () => {
  it('offers Push when the branch is ahead of its upstream', () => {
    expect(menuPushEnabled(status({ aheadCount: 2 }))).toBe(true);
  });

  it('does not offer Push when up to date with the upstream', () => {
    expect(menuPushEnabled(status({ hasUpstream: true, aheadCount: 0 }))).toBe(false);
  });

  // Porcelain reports no ahead count without an upstream; the push sets one.
  it('offers Push for a branch that has no upstream yet', () => {
    expect(menuPushEnabled(status({ branch: 'feature', hasUpstream: false, aheadCount: 0 }))).toBe(true);
  });

  it('does not offer Push on a detached HEAD without an upstream', () => {
    expect(menuPushEnabled(status({ branch: '', hasUpstream: false, aheadCount: 0 }))).toBe(false);
  });
});

describe('runRemoveWorktreeAction', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
  });

  it('removes the named worktree and applies the moved rows from the reply', async () => {
    const worktree = '/workspace/.worktrees/feature';
    const onWorktree = makeThread({ workspacePath: worktree, worktreePath: worktree, projectPath: '/workspace', branch: 'feature' });
    const pane = await buildPane(onWorktree);
    const moved = { ...onWorktree, workspacePath: '/workspace', worktreePath: '', branch: 'main' };
    const remove = setBindingMock('RemoveOtherWorktree', async () => ({
      workspace: { workspacePath: '/workspace', worktreePath: '', branch: 'main' },
      reattached: [moved],
    }));
    const c = removeCtx();
    await runRemoveWorktreeAction(c);
    // What this action owns is the RPC's subject: the checkout, plus the
    // worktree being removed.
    expect(remove).toHaveBeenCalledWith(WS, worktree, false);
    // The pane's own row is applied from the reply, not left to a
    // thread:updated event that can arrive after it.
    expect(pane.thread?.workspacePath).toBe('/workspace');
    expect(pane.thread?.worktreePath).toBe('');
    expect(pane.thread?.branch).toBe('main');
    // The moved row points the pane's status at the root; a refresh of the
    // removed checkout would read a directory that is gone.
    expect(c.refreshStatus).not.toHaveBeenCalled();
    expect(c.reportError).not.toHaveBeenCalled();
  });

  it('surfaces a thrown error via errString and does not refresh', async () => {
    const failure = new Error('worktree is dirty');
    setBindingMock('RemoveOtherWorktree', async () => {
      throw failure;
    });
    const c = removeCtx();
    await runRemoveWorktreeAction(c);
    expect(c.reportError).toHaveBeenCalledWith('Remove worktree failed: worktree is dirty', failure);
    expect(c.refreshStatus).not.toHaveBeenCalled();
  });
});
