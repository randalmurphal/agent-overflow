// "Check for Provider Updates" — the two halves of refreshing an imported
// thread from the session file it came from.
//
// Plain TS in the threadRowActions.ts style (no runes, no component context)
// so the copy mapping is testable on its own. The context menu owns only the
// confirm dialog between the two calls.
//
// The check is deliberately not silent about the verdicts it cannot act on.
// Six statuses come back and five of them mean "no refresh happened"; a UI
// that showed nothing for all five would be indistinguishable from a broken
// menu item. The backend ships user-facing prose for every one of them
// (internal/sessionimport/refresh.go), so it is what gets shown — the
// fallbacks below only cover a backend that sends none.

import { CheckThreadImportUpdates, ImportThreadUpdates } from '../../stores/bindings';
import { refreshSidebarProjections } from '../../stores/eventsThreadRows';
import { iterPanes } from '../../stores/panes.svelte';
import { addToast } from '../../stores/toast.svelte';
import type { ImportUpdateStatus } from '../../types/sessionImport';
import type { ToastType } from '../../stores/toast.svelte';
import { countNoun } from '../../utils/format';
import { userFacingError } from '../../utils/userFacingError';
import type { ThreadActionCtx } from './threadRowActions';

/**
 * What a status that cannot be applied should say when the backend sends no
 * prose of its own, and how loudly. `info` is "nothing to do here";
 * `warning` is "this thread can no longer track its source".
 */
const UNAPPLIABLE: Record<string, { tone: ToastType; fallback: string }> = {
  'up-to-date': { tone: 'info', fallback: 'No new items.' },
  'not-imported': {
    tone: 'info',
    fallback: 'This thread was created in Agent Overflow, not imported from a provider session.',
  },
  'diverged-local': {
    tone: 'warning',
    fallback: 'This thread has been continued in Agent Overflow since it was imported.',
  },
  'source-missing': {
    tone: 'warning',
    fallback: 'The session file this thread was imported from is gone.',
  },
  'source-diverged': {
    tone: 'warning',
    fallback: 'The session file this thread was imported from has changed incompatibly.',
  },
};

const UNKNOWN_STATUS: { tone: ToastType; fallback: string } = {
  tone: 'warning',
  fallback: 'Provider updates are not available for this thread.',
};

/**
 * Ask the backend whether the source session has grown.
 *
 * Returns the status ONLY when there is something to apply — every other
 * outcome has already been reported to the user by the time this resolves,
 * so a caller that sees `null` must not show anything further.
 *
 * `up-to-date` deliberately toasts rather than staying silent: the user
 * asked a question and "nothing changed" is the answer, not a no-op.
 */
export async function checkThreadImportUpdatesAction(
  ctx: ThreadActionCtx,
): Promise<ImportUpdateStatus | null> {
  let status: ImportUpdateStatus;
  try {
    status = await CheckThreadImportUpdates(ctx.thread.id);
  } catch (err) {
    console.error('Failed to check provider updates:', err);
    addToast('error', userFacingError(err));
    return null;
  }

  if (status?.status === 'updates-available') return status;

  const copy = UNAPPLIABLE[status?.status ?? ''] ?? UNKNOWN_STATUS;
  addToast(copy.tone, status?.detail || copy.fallback);
  return null;
}

/**
 * Append the source session's newer messages.
 *
 * The backend re-plans rather than trusting the status the check returned,
 * so a thread that moved in between refuses here instead of appending
 * against stale indices — which is why the failure path is a real toast and
 * not an assumed success.
 */
export async function applyThreadImportUpdatesAction(ctx: ThreadActionCtx): Promise<void> {
  const threadId = ctx.thread.id;
  try {
    const result = await ImportThreadUpdates(threadId);
    // Both halves of what landed: items are what the timeline gains, turns
    // are how many exchanges they came from. Reporting only the item count
    // discards a number the backend already computed and the user can see
    // in the thread a moment later.
    const across = result.appliedTurns > 0 ? ` across ${countNoun(result.appliedTurns, 'turn')}` : '';
    addToast('info', `Imported ${countNoun(result.appliedItems, 'new item')}${across}.`);
  } catch (err) {
    console.error('Failed to import provider updates:', err);
    addToast('error', userFacingError(err));
    return;
  }

  // Imported rows arrive without any per-item event (the append is a direct
  // store write), so both projections have to be pulled: the sidebar row for
  // its activity/preview, and any pane already showing the thread for the
  // items themselves. Per pane, not per thread — two panes on one thread can
  // hold different windows.
  refreshSidebarProjections();
  for (const pane of iterPanes()) {
    if (pane.threadId !== threadId) continue;
    void pane.refreshFromBackend();
  }
}
