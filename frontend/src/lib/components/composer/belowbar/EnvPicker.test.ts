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
  const pane = createThreadPane();
  await pane.switchThread(thread);
  return pane;
}

describe('<EnvPicker>', () => {
  beforeEach(() => resetBindingMocks());

  it('shows "Local" at the project root', async () => {
    const pane = await buildPane(makeThread({ workspacePath: '/repo' }));
    const { getByTestId } = render(EnvPicker, { props: { pane } });
    expect(getByTestId('env-picker-trigger').textContent ?? '').toMatch(/Local/);
  });

  it('shows the basename when on a worktree', async () => {
    const pane = await buildPane(
      makeThread({ workspacePath: '/tmp/wt-feature', projectPath: '/repo' }),
    );
    const { getByTestId } = render(EnvPicker, { props: { pane } });
    expect(getByTestId('env-picker-trigger').textContent ?? '').toMatch(/wt-feature/);
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
    const { getByTestId, findByRole } = render(EnvPicker, { props: { pane } });
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
});
