import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import BranchPicker from './BranchPicker.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import type { Thread } from '../../../types/models';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../../../test/mocks/bindings-app';

function makeThread(branch: string): Thread {
  return {
    id: 'thread-1',
    title: 'Test',
    provider: 'claude',
    workspacePath: '/repo',
    projectPath: '/repo',
    mode: 'chat',
    model: 'm',
    branch,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

async function buildPane(branch: string) {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(makeThread(branch));
  return pane;
}

describe('<BranchPicker>', () => {
  beforeEach(() => resetBindingMocks());

  it('renders the current branch on the trigger', async () => {
    const pane = await buildPane('main');
    const { getByTestId } = render(BranchPicker, { props: { pane } });
    expect(getByTestId('branch-picker-trigger').textContent ?? '').toMatch(/main/);
  });

  it('opens the dropdown and lists fetched branches', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isRemote: false, isCurrent: true, isDefault: true },
      { name: 'feat/abc', isRemote: false, isCurrent: false, isDefault: false },
    ]);
    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /feat\/abc/ });
    expect(row).toBeTruthy();
  });

  it('calls GitCheckout + UpdateThreadBranch on selection', async () => {
    const pane = await buildPane('main');
    setBindingMock('GitListBranches', async () => [
      { name: 'main', isRemote: false, isCurrent: true, isDefault: true },
      { name: 'feat/abc', isRemote: false, isCurrent: false, isDefault: false },
    ]);
    setBindingMock('GitCheckout', async () => {});
    setBindingMock('UpdateThreadBranch', async () => makeThread('feat/abc'));
    setBindingMock('GetThread', async () => makeThread('feat/abc'));

    const { getByTestId, findByRole } = render(BranchPicker, { props: { pane } });
    await fireEvent.click(getByTestId('branch-picker-trigger'));
    const row = await findByRole('menuitem', { name: /feat\/abc/ });
    await fireEvent.click(row);
    await Promise.resolve();
    await Promise.resolve();

    await waitFor(() => {
      expect(getBindingMock('GitCheckout')!.mock.calls[0]).toEqual(['thread-1', 'feat/abc']);
      expect(getBindingMock('UpdateThreadBranch')!.mock.calls[0]).toEqual([
        'thread-1',
        'feat/abc',
      ]);
    });
  });
});
