import { beforeEach, describe, expect, it } from 'vitest';
import { deleteSideChatThread } from './sideChatThread';
import { getThreadById, replaceAllThreads } from './threads.svelte';
import { getToasts, resetToastsForTest } from './toast.svelte';
import { makeThread } from '../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';

beforeEach(() => {
  resetBindingMocks();
  resetToastsForTest();
});

describe('deleteSideChatThread', () => {
  // The delete stands: the fork leaves the list, and what it could not stop
  // is said once, as a toast, since its pane is already gone.
  it('drops the fork and toasts what its delete could not stop', async () => {
    replaceAllThreads([makeThread({ id: 'fork', mode: 'scratch' })]);
    const message = 'The thread was deleted, but an ask it sent to another thread could not be stopped and may keep running.';
    setBindingMock('DeleteThread', async () => ({ cleanupError: message }));

    await deleteSideChatThread('fork');

    expect(getThreadById('fork')).toBeUndefined();
    expect(getToasts().map((t) => [t.type, t.message])).toEqual([['error', message]]);
  });

  it('drops the fork without a toast when the delete was clean', async () => {
    replaceAllThreads([makeThread({ id: 'fork', mode: 'scratch' })]);
    setBindingMock('DeleteThread', async () => ({}));

    await deleteSideChatThread('fork');

    expect(getThreadById('fork')).toBeUndefined();
    expect(getToasts()).toEqual([]);
  });
});
