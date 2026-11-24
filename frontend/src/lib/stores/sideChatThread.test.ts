import { describe, expect, it } from 'vitest';
import { deleteSideChatThread } from './sideChatThread';
import { getThreadById, replaceAllThreads } from './threads.svelte';
import { getToasts } from './toast.svelte';
import { makeThread } from '../../test/helpers/chat';
import { setBindingMock } from '../../test/mocks/bindings-app';

describe('deleteSideChatThread', () => {
  // The delete stands: the fork leaves the list, and what it could not stop
  // is said once, as a toast, since its pane is already gone.
  const message = 'The thread was deleted, but an ask it sent to another thread could not be stopped and may keep running.';
  it.each([
    ['drops the fork and toasts what its delete could not stop', { cleanupError: message }, [['error', message]]],
    ['drops the fork without a toast when the delete was clean', {}, []],
  ])('%s', async (_name, reply, toasts) => {
    replaceAllThreads([makeThread({ id: 'fork', mode: 'scratch' })]);
    setBindingMock('DeleteThread', async () => reply);

    await deleteSideChatThread('fork');

    expect(getThreadById('fork')).toBeUndefined();
    expect(getToasts().map((t) => [t.type, t.message])).toEqual(toasts);
  });
});
