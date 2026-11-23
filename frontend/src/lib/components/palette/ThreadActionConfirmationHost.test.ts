import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import ThreadActionConfirmationHost from './ThreadActionConfirmationHost.svelte';
import {
  requestThreadActionConfirmation,
} from '../../stores/threadActionConfirmations.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { makeThread } from '../../../test/helpers/chat';

function ctxFor(overrides: Parameters<typeof makeThread>[0]) {
  return {
    thread: makeThread(overrides),
    isActive: false,
    clearPane: vi.fn(),
    switchPane: vi.fn(async () => {}),
    reportError: vi.fn(),
  };
}

describe('<ThreadActionConfirmationHost>', () => {
  // The delete keeps the worktree, so the confirmation says nothing of it
  // and reads nothing about it.
  it('says nothing of the worktree of a thread on one', async () => {
    setBindingMock('GetDeleteDependents', async () => ({ pastThreads: 0, helpers: 0, workers: 0 }));
    const status = setBindingMock('GitWorktreeStatus', vi.fn(async () => ({ terminals: 1 })));
    const { findByText } = render(ThreadActionConfirmationHost);

    requestThreadActionConfirmation('delete', ctxFor({ workspacePath: '/tmp/wt/palette', worktreePath: '/tmp/wt/palette' }));

    expect((await findByText(/This action cannot be undone\./)).textContent).toBe(
      'This will permanently delete this thread and all its messages. This action cannot be undone.',
    );
    expect(status).not.toHaveBeenCalled();
  });

  // Unknown is not none: the palette's delete cannot be confirmed until it
  // knows what the delete takes, and a read that failed says so.
  it('cannot be confirmed before the read answers, and says when the read failed', async () => {
    let fail: (err: Error) => void = () => {};
    setBindingMock('GetDeleteDependents', () => new Promise((_resolve, reject) => { fail = reject; }));
    const remove = setBindingMock('DeleteThreads', vi.fn(async () => ({})));
    const view = render(ThreadActionConfirmationHost);
    requestThreadActionConfirmation('delete', ctxFor({ id: 'palette-delete' }));
    const confirm = async () => (await view.findByRole('button', { name: 'Delete' })) as HTMLButtonElement;
    expect((await confirm()).disabled).toBe(true);
    await fireEvent.click(await confirm());
    expect(remove).not.toHaveBeenCalled();
    fail(new Error('database is locked'));
    await waitFor(async () => expect((await confirm()).disabled).toBe(false));
    expect((await view.findByText(/This action cannot be undone\./)).textContent).toBe(
      'This will permanently delete this thread and all its messages. This action cannot be undone. Its sub-threads could not be checked; any it has are deleted too, here and on other computers.',
    );
    await fireEvent.click(await confirm());
    await waitFor(() => expect(remove).toHaveBeenCalledWith(['palette-delete']));
  });
});
