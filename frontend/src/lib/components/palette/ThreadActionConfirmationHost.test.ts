import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import ThreadActionConfirmationHost from './ThreadActionConfirmationHost.svelte';
import {
  requestThreadActionConfirmation,
  resetThreadActionConfirmationsForTest,
} from '../../stores/threadActionConfirmations.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
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

beforeEach(() => {
  resetBindingMocks();
  resetThreadActionConfirmationsForTest();
});

afterEach(() => resetThreadActionConfirmationsForTest());

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
});
