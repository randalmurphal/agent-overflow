// Draft event domain: converging the composer across attached clients on
// `draft:updated`. Fan-in target of events.ts's setupEventListeners.
//
// The backend emits one frame per persisted draft write — a save, a clear, a
// send consuming the row, a saga restoring one — naming the thread and the
// screen that wrote it. It never carries the text; receivers re-read.
import { getComposerDraftForPane } from './composerDraftRegistry.svelte';
import { hasRememberedDraftSnapshot } from './composerDraftSnapshots';
import { iterPanes } from './panes.svelte';
import type { ThreadPaneIngest } from './threadPaneRoles';
import { getConnectionId } from '../transport/clientIdentity';

// The registry hands out whole ThreadPanes; this module narrows them to
// the ingest surface at the one acquisition point, so a new pane member
// use here fails to compile until threadPaneRoles.ts lists it.
function ingestPanes(): Iterable<ThreadPaneIngest> {
  return iterPanes();
}

/**
 * `draft:updated` — the thread's draft row moved, and this is who moved it.
 *
 * Carries no draft text: `GetDraft` takes `threads:operate` because a composer
 * holds in-progress user work, and a push carrying that text would be the one
 * path around the grant that read enforces. The frame is a re-read nudge.
 */
export interface DraftUpdatedEvent {
  threadId: string;
  updatedAt: number;
  /** Durable per browser profile. For attribution, never for suppression. */
  deviceId?: string;
  /** Unique per page load. The suppression key. */
  connectionId?: string;
}

/**
 * Converge the composer on a draft another client wrote.
 *
 * Three reasons a frame is dropped, in the order they are checked:
 *
 *  1. **It is our own echo.** Every save this client makes comes back as a
 *     frame. Re-reading on it would replace the composer's live text with a
 *     round-tripped copy of itself mid-keystroke. Suppression is keyed on the
 *     CONNECTION, not the device: two tabs of one browser share a device id,
 *     and keying on that would leave each sitting on the other's stale text.
 *
 *  2. **We hold unsaved work for the thread.** The remote write is not the
 *     last write — this client's pending save is, and it lands in a debounce
 *     tick. Adopting the remote text here would delete characters out from
 *     under someone who is still typing them. Last write wins, and the local
 *     one has not happened yet.
 *
 *  3. **No pane is showing the thread.** There is nothing to converge. The
 *     next open hydrates from the row, which is by then the remote's.
 *
 * Otherwise every pane on the thread re-reads. Deletes and edits take the same
 * path: `GetDraft` on a cleared thread comes back empty, which is what the
 * composer should show.
 *
 * Concurrent frames are safe to fire in parallel — `reloadFromBackend` bumps
 * the store's switch generation, so a slower earlier read is discarded rather
 * than painted over a newer one.
 */
export function applyDraftUpdated(evt: DraftUpdatedEvent | undefined): void {
  if (!evt?.threadId) return;
  if (evt.connectionId && evt.connectionId === getConnectionId()) return;
  if (hasRememberedDraftSnapshot(evt.threadId)) return;

  for (const pane of ingestPanes()) {
    if (pane.threadId !== evt.threadId) continue;
    const draft = getComposerDraftForPane(pane.paneId);
    if (!draft) continue;
    void draft.reloadFromBackend(evt.threadId);
  }
}
