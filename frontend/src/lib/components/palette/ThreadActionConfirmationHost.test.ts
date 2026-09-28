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
  it('says how many terminals deleting a thread on a worktree closes', async () => {
    setBindingMock('GitWorktreeStatus', async () => ({ path: '/tmp/wt/palette', terminals: 1 }));
    const { findByText } = render(ThreadActionConfirmationHost);

    requestThreadActionConfirmation('delete', ctxFor({ workspacePath: '/tmp/wt/palette', worktreePath: '/tmp/wt/palette' }));

    await findByText(/This action cannot be undone\. 1 terminal will close\./);
  });

  it('adds nothing for a thread at the project root', async () => {
    const status = setBindingMock('GitWorktreeStatus', async () => ({ terminals: 4 }));
    const { findByText, queryByText } = render(ThreadActionConfirmationHost);

    requestThreadActionConfirmation('delete', ctxFor({ worktreePath: '' }));

    await findByText(/This action cannot be undone\./);
    expect(queryByText(/will close/)).toBeNull();
    expect(status).not.toHaveBeenCalled();
  });
});
