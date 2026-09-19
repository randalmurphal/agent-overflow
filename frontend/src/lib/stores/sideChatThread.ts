// Deleting a side chat's scratch thread.
//
// Separate from `sideChat.ts` because the companion registry calls it:
// closing a `side-chat` companion deletes the fork, and this module imports
// nothing from that registry, so the two do not form a cycle.

import { DeleteThread } from './bindings';
import { removeThread } from './threads.svelte';
import { addToast } from './toast.svelte';
import { userFacingError } from '../utils/userFacingError';

/**
 * Delete a side chat's fork. The caller has already taken its pane down.
 *
 * A failure is a toast rather than an inline error: the pane the text would
 * have appeared next to is gone, and a fork that outlives its pane is a
 * hidden row nothing but the next boot sweep would clear.
 */
export async function deleteSideChatThread(threadId: string): Promise<void> {
  if (!threadId) return;
  try {
    await DeleteThread(threadId);
    removeThread(threadId);
  } catch (err) {
    console.error('Failed to delete the side chat thread:', err);
    addToast(
      'error',
      userFacingError(err, 'The side chat closed but its thread could not be deleted.'),
    );
  }
}
