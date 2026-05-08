import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import EnvPicker from './EnvPicker.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import type { Thread } from '../../../types/models';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../../../test/mocks/bindings-app';
import { resetForTest as resetWorktreeIntent } from '../../../stores/worktreeIntent.svelte';
import type { WorkspaceChangeLockState } from '../../../stores/workspaceChangeLock.svelte';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test',
    provider: 'claude',
    workspacePath: '/repo',
    projectPath: '/repo',
    mode: 'chat',
    model: 'm',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

async function buildPane(thread: Thread) {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  setBindingMock('ListLiveBackgroundTasks', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(thread);
  return pane;
}

function makeWorkspaceLock(overrides: Partial<WorkspaceChangeLockState> = {}): WorkspaceChangeLockState {
  return {
    locked: false,
    reason: '',
    runningBackgroundCount: 0,
    refresh: async () => {},
    ...overrides,
  };
}

describe('<EnvPicker>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWorktreeIntent();
  });

  it('shows the current checkout at the project root', async () => {
    const pane = await buildPane(makeThread({ workspacePath: '/repo' }));
    const { getByTestId } = render(EnvPicker, { props: { pane, workspaceLock: makeWorkspaceLock() } });
    expect(getByTestId('env-picker-trigger').textContent ?? '').toMatch(/Current Checkout/);
  });

  it('shows when the thread is on a worktree', async () => {
    const pane = await buildPane(
      makeThread({ workspacePath: '/tmp/wt-feature', projectPath: '/repo' }),
    );
    const { getByTestId } = render(EnvPicker, { props: { pane, workspaceLock: makeWorkspaceLock() } });
    expect(getByTestId('env-picker-trigger').textContent ?? '').toMatch(/Current Worktree/);
  });

  it('lists worktrees on open and switches via UpdateThreadWorkspace', async () => {
    const pane = await buildPane(makeThread({ workspacePath: '/repo', projectPath: '/repo' }));
    setBindingMock('GitListWorktrees', async () => [
      { path: '/repo', branch: 'main', head: 'abc' },
      { path: '/tmp/wt-feature', branch: 'feat', head: 'def' },
    ]);
    setBindingMock('UpdateThreadWorkspace', async () =>
      makeThread({ workspacePath: '/tmp/wt-feature' }),
    );
    const { getByTestId, findByRole } = render(EnvPicker, { props: { pane, workspaceLock: makeWorkspaceLock() } });
    await fireEvent.click(getByTestId('env-picker-trigger'));

    const wtRow = await findByRole('menuitem', { name: /wt-feature/ });
    await fireEvent.click(wtRow);
    await Promise.resolve();
    await Promise.resolve();

    await waitFor(() => {
      const call = getBindingMock('UpdateThreadWorkspace')?.mock.calls[0];
      expect(call).toEqual(['thread-1', '/tmp/wt-feature']);
    });
  });

  it('stages a new worktree without switching immediately', async () => {
    const pane = await buildPane(makeThread({ workspacePath: '/repo', projectPath: '/repo' }));
    setBindingMock('GitListWorktrees', async () => [{ path: '/repo', branch: 'main', head: 'abc' }]);

    const { getByTestId, findByRole } = render(EnvPicker, { props: { pane, workspaceLock: makeWorkspaceLock() } });
    await fireEvent.click(getByTestId('env-picker-trigger'));
    const row = await findByRole('menuitem', { name: /New Worktree/ });
    await fireEvent.click(row);

    expect(getByTestId('env-picker-trigger').textContent ?? '').toMatch(/New Worktree/);
    expect(getBindingMock('UpdateThreadWorkspace')).toBeUndefined();
  });

  it('disables workspace changes while the agent is responding', async () => {
    const pane = await buildPane(makeThread({ workspacePath: '/repo', projectPath: '/repo' }));
    const workspaceLock = makeWorkspaceLock({
      locked: true,
      reason: 'Workspace changes are unavailable while the agent is responding.',
    });
    setBindingMock('GitListWorktrees', async () => [
      { path: '/repo', branch: 'main', head: 'abc' },
      { path: '/tmp/wt-feature', branch: 'feat', head: 'def' },
    ]);
    setBindingMock('UpdateThreadWorkspace', async () =>
      makeThread({ workspacePath: '/tmp/wt-feature' }),
    );

    const { getByTestId, findByRole } = render(EnvPicker, { props: { pane, workspaceLock } });
    await fireEvent.click(getByTestId('env-picker-trigger'));
    const newWorktreeRow = await findByRole('menuitem', { name: /New Worktree/ });
    expect(newWorktreeRow).toHaveAttribute('aria-disabled', 'true');
    const wtRow = await findByRole('menuitem', { name: /wt-feature/ });
    expect(wtRow).toHaveAttribute('aria-disabled', 'true');
    expect(wtRow).toHaveAttribute('title', expect.stringMatching(/agent is responding/));

    await fireEvent.click(wtRow);
    await fireEvent.click(newWorktreeRow);

    expect(getBindingMock('UpdateThreadWorkspace')).not.toHaveBeenCalled();
    expect(getByTestId('env-picker-trigger').textContent ?? '').toMatch(/Current Checkout/);
  });

  it('disables workspace changes while background tasks are running', async () => {
    const pane = await buildPane(makeThread({ workspacePath: '/repo', projectPath: '/repo' }));
    const workspaceLock = makeWorkspaceLock({
      locked: true,
      reason: 'Workspace changes are unavailable while background tasks are running.',
      runningBackgroundCount: 1,
    });
    setBindingMock('GitListWorktrees', async () => [
      { path: '/repo', branch: 'main', head: 'abc' },
      { path: '/tmp/wt-feature', branch: 'feat', head: 'def' },
    ]);
    setBindingMock('UpdateThreadWorkspace', async () =>
      makeThread({ workspacePath: '/tmp/wt-feature' }),
    );

    const { getByTestId, findByRole } = render(EnvPicker, { props: { pane, workspaceLock } });
    await fireEvent.click(getByTestId('env-picker-trigger'));
    const newWorktreeRow = await findByRole('menuitem', { name: /New Worktree/ });
    expect(newWorktreeRow).toHaveAttribute('aria-disabled', 'true');
    const wtRow = await findByRole('menuitem', { name: /wt-feature/ });

    await waitFor(() => {
      expect(wtRow).toHaveAttribute('aria-disabled', 'true');
      expect(wtRow).toHaveAttribute('title', expect.stringMatching(/background tasks/));
    });

    await fireEvent.click(wtRow);
    await fireEvent.click(newWorktreeRow);

    expect(getBindingMock('UpdateThreadWorkspace')).not.toHaveBeenCalled();
    expect(getByTestId('env-picker-trigger').textContent ?? '').toMatch(/Current Checkout/);
  });
});
