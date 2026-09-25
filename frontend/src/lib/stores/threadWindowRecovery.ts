import { refreshTimelineSurfaces } from './timelineSurfaces';
import { threadItemCache } from './threadItemCache';
import { iterPanes } from './panes.svelte';
import type { ThreadPaneIngest } from './threadPaneRoles';
import { holdBackendRecovery } from './transportRecovery';
import { threadMachine } from './attachedBackends.svelte';

// The registry hands out whole ThreadPanes; this module narrows them to
// the ingest surface at the one acquisition point, so a new pane member
// use here fails to compile until threadPaneRoles.ts lists it.
function ingestPanes(): Iterable<ThreadPaneIngest> {
  return iterPanes();
}

/**
 * Re-read what this client shows of `threads` from the backend: every
 * pane's window, every timeline surface, and no cached stamp that does not
 * attest its own rows. For threads whose history changed without the item
 * frames that would have described it, which a transport gap lost.
 * Everything an item event updates is keyed by its own thread
 * (`applyItemStreamEvent`), so every other thread is as current as before.
 */
export function recoverThreadWindows(threads: ReadonlySet<string>): void {
  refreshTimelineSurfaces(threads);
  threadItemCache.dropUnattestedStamps(threads);
  for (const pane of ingestPanes()) {
    if (!pane.threadId || !threads.has(pane.threadId)) continue;
    holdBackendRecovery(threadMachine(pane.threadId, pane.thread?.projectId), pane.refreshFromBackend());
  }
}
