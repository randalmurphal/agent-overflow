// The one report of what an archive or delete that went through could not
// stop. Separate from `threadRowActions.ts` so the side chat's delete
// (sideChatThread.ts), which imports nothing from the sidebar, shares it.

import { addErrorToast } from './toast.svelte';

/**
 * Show `cleanupError` (ArchiveThreadResult, DeleteThreadResult), the
 * backend's sentence for a person on what the archived or deleted threads
 * started that may still be running or open. The archive or delete stands,
 * so the caller drops the thread as on a clean result. A toast, not an
 * inline error: the pane that would show it may be the one just closed, and
 * a bulk caller would count it as not done.
 */
export function reportThreadCleanupError(result: { cleanupError?: string }): void {
  if (result.cleanupError) addErrorToast(result.cleanupError, result.cleanupError);
}
