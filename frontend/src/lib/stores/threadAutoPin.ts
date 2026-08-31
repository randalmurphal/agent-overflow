// Auto-pin policy for threads the user explicitly starts in-app.
//
// A normal new thread is a materialized draft until its first successful
// SendMessageWithOptions response. Forks arrive with copied history and are
// therefore already started at creation. Both paths call autoPinNewThread only
// after that transition succeeds; imports, provider-spawned children, later
// sends, and abandoned drafts never reach the pin RPC.

import { PinThread } from './bindings';
import { getSettings } from './settings.svelte';
import { addToast } from './toast.svelte';
import type { Thread } from '../types/models';
import { isHiddenThreadMode } from '../utils/threadModes';
import { userFacingError } from '../utils/userFacingError';

export function shouldAutoPinFirstSend(thread: Thread | undefined): boolean {
  return getSettings().autoPinNewThreads
    && thread?.isDraft === true
    && !thread.importSource
    && !thread.parentThreadId
    && !isHiddenThreadMode(thread.mode);
}

/**
 * Pin a successfully-started in-app thread on the front burner. A pin failure
 * is surfaced separately and never reclassifies the already-successful create
 * or send as failed.
 */
export async function autoPinNewThread(thread: Thread): Promise<Thread> {
  if (!getSettings().autoPinNewThreads) return thread;
  try {
    return await PinThread(thread.id) as Thread;
  } catch (err) {
    console.error('Failed to auto-pin new thread:', err);
    addToast('error', `Thread started, but auto-pin failed: ${userFacingError(err)}`);
    return thread;
  }
}
