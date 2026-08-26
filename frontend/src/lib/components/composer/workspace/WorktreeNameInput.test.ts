import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import WorktreeNameInput from './WorktreeNameInput.svelte';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../../../test/mocks/bindings-app';
import {
  enterCreateBranchMode,
  resetForTest as resetWorktreeIntent,
  setNewBranchName,
  setThreadEnvMode,
  worktreeIntentForThread,
} from '../../../stores/worktreeIntent.svelte';
import { resetWorktreeIntentMaterializeForTest } from '../../../stores/worktreeIntentMaterialize';
import { buildPane, makeThread } from '../../../../test/helpers/chat';
import { idleWorkspaceActivity, makeWorkspaceLock } from '../../../../test/helpers/workspaceLock';

// Workspace interaction materializes a draft before this component can apply
// anything. Draft and committed rows therefore use the same thread-scoped
// engine and expose the same postcondition: the row owns the checkout and the
// staged control is gone.
function threadOverrides() {
  return {
    branch: 'main',
    workspacePath: '/repo',
    projectPath: '/repo',
    projectId: 'project-1',
    isDraft: true,
  };
}

async function buildWorktreePane(overrides: Record<string, unknown> = {}) {
  setBindingMock('ListLiveBackgroundTasks', async () => []);
  setBindingMock('GetWorkspaceActivity', async () => idleWorkspaceActivity());
  return buildPane(makeThread({ ...threadOverrides(), ...overrides }));
}

describe('<WorktreeNameInput>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWorktreeIntent();
    resetWorktreeIntentMaterializeForTest();
  });

  it('renders nothing without a staged intent', async () => {
    const pane = await buildWorktreePane();
    const { queryByTestId } = render(WorktreeNameInput, {
      props: { pane, workspaceDirty: false, workspaceLock: makeWorkspaceLock() },
    });
    expect(queryByTestId('apply-worktree-intent-button')).toBeNull();
    expect(queryByTestId('new-branch-toggle')).toBeNull();
  });

  it('moves a materialized draft into the worktree and clears the create control', async () => {
    const pane = await buildWorktreePane();
    setThreadEnvMode(pane.thread!, 'new-worktree');
    const attach = setBindingMock('AttachThreadWorktree', async () => ({
      ...pane.thread,
      worktreePath: '/wt/main',
      workspacePath: '/wt/main',
      branch: 'main',
    }));

    const { getByTestId, queryByTestId } = render(WorktreeNameInput, {
      props: { pane, workspaceDirty: false, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('apply-worktree-intent-button'));

    await waitFor(() => {
      expect(attach).toHaveBeenCalledWith('thread-1', 'main');
      expect(pane.thread?.workspacePath).toBe('/wt/main');
      expect(queryByTestId('apply-worktree-intent-button')).toBeNull();
    });
  });

  it('applies a staged new-branch worktree intent with the typed name', async () => {
    const pane = await buildWorktreePane();
    setThreadEnvMode(pane.thread!, 'new-worktree');
    enterCreateBranchMode(pane.thread!, { workspaceDirty: false, currentBranch: 'main' });
    setNewBranchName(pane.thread!, 'feat/button');
    const prepare = setBindingMock('PrepareThreadWorktree', async () => ({
      ...pane.thread,
      worktreePath: '/wt/feat-button',
      workspacePath: '/wt/feat-button',
      branch: 'feat/button',
    }));

    const { getByTestId, queryByTestId } = render(WorktreeNameInput, {
      props: { pane, workspaceDirty: false, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('apply-worktree-intent-button'));

    await waitFor(() => {
      expect(prepare.mock.calls[0]).toEqual([
        'thread-1',
        'main',
        'feat/button',
        false,
      ]);
      expect(pane.thread?.branch).toBe('feat/button');
      expect(queryByTestId('apply-worktree-intent-button')).toBeNull();
    });
  });

  it('applies on Enter inside the branch-name input', async () => {
    const pane = await buildWorktreePane();
    enterCreateBranchMode(pane.thread!, { workspaceDirty: false, currentBranch: 'main' });
    setNewBranchName(pane.thread!, 'feat/enter');
    const create = setBindingMock('GitCreateBranchFrom', async () => ({
      ...pane.thread,
      worktreePath: '',
      workspacePath: '/repo',
      branch: 'feat/enter',
    }));

    const { getByTestId } = render(WorktreeNameInput, {
      props: { pane, workspaceDirty: false, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.keyDown(getByTestId('worktree-branch-name-input'), { key: 'Enter' });

    await waitFor(() => {
      expect(create.mock.calls[0]).toEqual([
        'thread-1',
        'feat/enter',
        'main',
        false,
      ]);
    });
  });

  it('disables the confirm button while THIS thread is locked, with the reason as title', async () => {
    const pane = await buildWorktreePane();
    setThreadEnvMode(pane.thread!, 'new-worktree');
    const { getByTestId } = render(WorktreeNameInput, {
      props: {
        pane,
        workspaceDirty: false,
        workspaceLock: makeWorkspaceLock({
          locked: true,
          reason: 'turn 0 is active',
          threadLocked: true,
          threadReason: 'turn 0 is active',
        }),
      },
    });
    const button = getByTestId('apply-worktree-intent-button') as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    expect(button.title).toBe('turn 0 is active');
  });

  // A new worktree is a new directory; a sibling busy in the current one is
  // no reason to refuse cutting it.
  it('keeps the new-worktree confirm live when only the directory is busy', async () => {
    const pane = await buildWorktreePane();
    setThreadEnvMode(pane.thread!, 'new-worktree');
    const { getByTestId } = render(WorktreeNameInput, {
      props: {
        pane,
        workspaceDirty: false,
        workspaceLock: makeWorkspaceLock({ locked: true, reason: 'turn 0 is active' }),
      },
    });
    const button = getByTestId('apply-worktree-intent-button') as HTMLButtonElement;
    expect(button.disabled).toBe(false);
  });

  // A local branch create moves HEAD under every thread sharing the checkout,
  // so the directory view gates it even when this thread is idle.
  it('disables a local branch create while the directory is busy', async () => {
    const pane = await buildWorktreePane();
    enterCreateBranchMode(pane.thread!, { workspaceDirty: false, currentBranch: 'main' });
    setNewBranchName(pane.thread!, 'feat');
    const { getByTestId } = render(WorktreeNameInput, {
      props: {
        pane,
        workspaceDirty: false,
        workspaceLock: makeWorkspaceLock({ locked: true, reason: 'turn 0 is active' }),
      },
    });
    const button = getByTestId('apply-worktree-intent-button') as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    expect(button.title).toBe('turn 0 is active');
  });

  it('disables the confirm button when the branch name is empty', async () => {
    const pane = await buildWorktreePane();
    enterCreateBranchMode(pane.thread!, { workspaceDirty: false, currentBranch: 'main' });
    const { getByTestId } = render(WorktreeNameInput, {
      props: { pane, workspaceDirty: false, workspaceLock: makeWorkspaceLock() },
    });
    const button = getByTestId('apply-worktree-intent-button') as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    expect(button.title).toBe('Enter a branch name first');
  });

  it('ignores repeat clicks while a slow apply is in flight', async () => {
    const pane = await buildWorktreePane();
    setThreadEnvMode(pane.thread!, 'new-worktree');
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const attach = setBindingMock('AttachThreadWorktree', async () => {
      await gate;
      return { ...pane.thread, worktreePath: '/wt/main', workspacePath: '/wt/main', branch: 'main' };
    });

    const { getByTestId } = render(WorktreeNameInput, {
      props: { pane, workspaceDirty: false, workspaceLock: makeWorkspaceLock() },
    });
    const button = getByTestId('apply-worktree-intent-button');
    await fireEvent.click(button);
    await fireEvent.click(button);
    release();

    await waitFor(() => expect(pane.thread?.workspacePath).toBe('/wt/main'));
    expect(attach).toHaveBeenCalledTimes(1);
  });

  it('keeps the staged intent when the backend refuses', async () => {
    const pane = await buildWorktreePane();
    setThreadEnvMode(pane.thread!, 'new-worktree');
    const attach = setBindingMock('AttachThreadWorktree', async () => {
      throw new Error('worktree create failed: branch busy');
    });

    const { getByTestId } = render(WorktreeNameInput, {
      props: { pane, workspaceDirty: false, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('apply-worktree-intent-button'));

    await waitFor(() => {
      expect(attach).toHaveBeenCalledTimes(1);
      expect(
        (getByTestId('apply-worktree-intent-button') as HTMLButtonElement).disabled,
      ).toBe(false);
    });
    expect(worktreeIntentForThread(pane.thread).mode).toBe('new-worktree');
  });

  it('routes a thread with history through the thread-scoped engine', async () => {
    const pane = await buildWorktreePane({ isDraft: false });
    setThreadEnvMode(pane.thread!, 'new-worktree');
    setBindingMock('AttachThreadWorktree', async () => ({
      ...pane.thread,
      worktreePath: '/wt/main',
      workspacePath: '/wt/main',
      branch: 'main',
    }));

    const { getByTestId } = render(WorktreeNameInput, {
      props: { pane, workspaceDirty: false, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('apply-worktree-intent-button'));

    await waitFor(() => {
      expect(getBindingMock('AttachThreadWorktree')!.mock.calls[0]).toEqual(['thread-1', 'main']);
    });
    // The thread-scoped engine moved the row itself, so nothing is left
    // pending.
    expect(worktreeIntentForThread(pane.thread).mode).toBe('local');
  });
});
