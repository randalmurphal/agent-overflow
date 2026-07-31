import { beforeEach, describe, expect, it } from 'vitest';

import {
  materializeWorktreeIntentOnThread,
  prepareThreadWorktreeIntent,
} from './worktreeIntentMaterialize';
import {
  resetForTest as resetWorktreeIntent,
  setThreadEnvMode,
  worktreeIntentForThread,
} from './worktreeIntent.svelte';
import { recentBranchSelections } from './branchMru';
import { resetAppStorageForTest } from './appStorage';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../test/mocks/bindings-app';
import { makeThread } from '../../test/helpers/chat';

function stagedThread() {
  const thread = makeThread({
    id: 'thread-mat',
    branch: 'main',
    workspacePath: '/repo',
    projectPath: '/repo',
    projectId: 'project-1',
  });
  setThreadEnvMode(thread, 'new-worktree');
  return thread;
}

describe('worktreeIntentMaterialize', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWorktreeIntent();
    resetAppStorageForTest();
  });

  it('coalesces concurrent prepares for the same thread into one backend call', async () => {
    const thread = stagedThread();
    const updated = makeThread({
      ...thread,
      branch: 'main',
      worktreePath: '/wt/main',
    });
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    setBindingMock('AttachThreadWorktree', async () => {
      await gate;
      return updated;
    });

    // The apply-now button and a send racing each other: both must
    // resolve to the winner's result off a single backend call.
    const first = prepareThreadWorktreeIntent({ thread });
    const second = prepareThreadWorktreeIntent({ thread });
    release();
    const [firstResult, secondResult] = await Promise.all([first, second]);

    expect(getBindingMock('AttachThreadWorktree')!.mock.calls.length).toBe(1);
    expect(firstResult?.worktreePath).toBe('/wt/main');
    expect(secondResult).toBe(firstResult);
    expect(worktreeIntentForThread(thread).mode).toBe('local');
  });

  it('allows a fresh prepare after the in-flight one settles', async () => {
    const thread = stagedThread();
    setBindingMock('AttachThreadWorktree', async () => {
      throw new Error('worktree create failed: branch busy');
    });

    await expect(prepareThreadWorktreeIntent({ thread })).rejects.toThrow('branch busy');
    // The failed attempt must not stay cached as the in-flight winner.
    await expect(prepareThreadWorktreeIntent({ thread })).rejects.toThrow('branch busy');
    expect(getBindingMock('AttachThreadWorktree')!.mock.calls.length).toBe(2);
    expect(worktreeIntentForThread(thread).mode).toBe('new-worktree');
  });

  it('records the materialized branch in the project MRU on success', async () => {
    const thread = makeThread({
      id: 'thread-mru',
      branch: 'main',
      workspacePath: '/repo',
      projectPath: '/repo',
      projectId: 'project-1',
    });
    const updated = makeThread({ ...thread, branch: 'feat/new' });
    setBindingMock('GitCreateBranchFrom', async () => updated);

    const result = await materializeWorktreeIntentOnThread({
      targetThread: thread,
      intent: {
        mode: 'local',
        creatingBranch: true,
        newBranchName: 'feat/new',
        newBranchBase: 'main',
        attachBranch: '',
      },
    });

    expect(result?.branch).toBe('feat/new');
    expect(recentBranchSelections('project-1')).toEqual(['feat/new']);
  });
});
