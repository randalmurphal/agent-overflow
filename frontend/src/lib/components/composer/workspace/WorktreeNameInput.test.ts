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
import { makeWorkspaceLock } from '../../../../test/helpers/workspaceLock';

// The confirm button's owner in these tests is a materialized DRAFT row —
// the shape the composer's workspace strip is normally staged on. Ownership
// is what picks the engine (see worktreeIntentMaterialize.ts): a draft goes
// project-scoped and leaves the row where it is; a thread with history goes
// thread-scoped, which the last test in this file covers.
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
  setBindingMock('GetWorkspaceActivity', async () => ({ activeTurnThreads: 0, runningBackgroundTasks: 0 }));
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

  it('applies a staged attach-worktree intent from the confirm button', async () => {
    const pane = await buildWorktreePane();
    setThreadEnvMode(pane.thread!, 'new-worktree');
    setBindingMock('AttachProjectWorktree', async () => ({
      worktreePath: '/wt/main',
      branch: 'main',
    }));

    const { getByTestId } = render(WorktreeNameInput, {
      props: { pane, workspaceDirty: false, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('apply-worktree-intent-button'));

    await waitFor(() => {
      // Project-scoped: the worktree exists after this, but the THREAD is not
      // moved onto it until a send binds it.
      expect(getBindingMock('AttachProjectWorktree')!.mock.calls[0]).toEqual(['project-1', 'main']);
      expect(worktreeIntentForThread(pane.thread).applied).toEqual({
        worktreePath: '/wt/main',
        branch: 'main',
      });
    });
  });

  it('applies a staged new-branch worktree intent with the typed name', async () => {
    const pane = await buildWorktreePane();
    setThreadEnvMode(pane.thread!, 'new-worktree');
    enterCreateBranchMode(pane.thread!, { workspaceDirty: false, currentBranch: 'main' });
    setNewBranchName(pane.thread!, 'feat/button');
    setBindingMock('PrepareProjectWorktree', async () => ({
      worktreePath: '/wt/feat-button',
      branch: 'feat/button',
    }));

    const { getByTestId } = render(WorktreeNameInput, {
      props: { pane, workspaceDirty: false, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('apply-worktree-intent-button'));

    await waitFor(() => {
      expect(getBindingMock('PrepareProjectWorktree')!.mock.calls[0]).toEqual([
        'project-1',
        'main',
        'feat/button',
        false,
        '/repo',
      ]);
      expect(worktreeIntentForThread(pane.thread).applied?.branch).toBe('feat/button');
    });
  });

  it('applies on Enter inside the branch-name input', async () => {
    const pane = await buildWorktreePane();
    enterCreateBranchMode(pane.thread!, { workspaceDirty: false, currentBranch: 'main' });
    setNewBranchName(pane.thread!, 'feat/enter');
    setBindingMock('CreateProjectBranch', async () => ({ worktreePath: '', branch: 'feat/enter' }));

    const { getByTestId } = render(WorktreeNameInput, {
      props: { pane, workspaceDirty: false, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.keyDown(getByTestId('worktree-branch-name-input'), { key: 'Enter' });

    await waitFor(() => {
      expect(getBindingMock('CreateProjectBranch')!.mock.calls[0]).toEqual([
        'project-1',
        'feat/enter',
        'main',
        false,
        '/repo',
      ]);
    });
  });

  it('disables the confirm button while the workspace is locked, with the reason as title', async () => {
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
    setBindingMock('AttachProjectWorktree', async () => {
      await gate;
      return { worktreePath: '/wt/main', branch: 'main' };
    });

    const { getByTestId } = render(WorktreeNameInput, {
      props: { pane, workspaceDirty: false, workspaceLock: makeWorkspaceLock() },
    });
    const button = getByTestId('apply-worktree-intent-button');
    await fireEvent.click(button);
    await fireEvent.click(button);
    release();

    await waitFor(() => {
      expect(worktreeIntentForThread(pane.thread).applied).not.toBeNull();
    });
    expect(getBindingMock('AttachProjectWorktree')!.mock.calls.length).toBe(1);
  });

  it('keeps the staged intent when the backend refuses', async () => {
    const pane = await buildWorktreePane();
    setThreadEnvMode(pane.thread!, 'new-worktree');
    setBindingMock('AttachProjectWorktree', async () => {
      throw new Error('worktree create failed: branch busy');
    });

    const { getByTestId } = render(WorktreeNameInput, {
      props: { pane, workspaceDirty: false, workspaceLock: makeWorkspaceLock() },
    });
    await fireEvent.click(getByTestId('apply-worktree-intent-button'));

    await waitFor(() => {
      expect(getBindingMock('AttachProjectWorktree')!.mock.calls.length).toBe(1);
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
    // pending: the intent is cleared and `applied` is never set.
    expect(getBindingMock('AttachProjectWorktree')).toBeUndefined();
    expect(worktreeIntentForThread(pane.thread).applied).toBeNull();
    expect(worktreeIntentForThread(pane.thread).mode).toBe('local');
  });
});
