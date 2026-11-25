// Opening a ticket in the Issues pane from outside it: a thread's linked
// ticket, a pull request's chip. The Issues pane's stores load with the
// pane, so the request that tells it which ticket to open is reached
// through one stable dynamic import (architecture.test.ts, rule 4b).

import { attachedBackendEntry, backendDisplayName } from './attachedBackends.svelte';
import { addErrorToast, addToast } from './toast.svelte';
import { openTool } from './toolPanes.svelte';
import { userFacingError } from '../utils/userFacingError';
import { retryingLoad } from '../utils/retryingLoad';
import type { BackendKey } from '../transport/backendKey';

const loadIssuesPane = retryingLoad(() => import('./issuesPane.svelte'));

/** A ticket to open: its computer, connection and issue, its key and title
 * as the opener shows them, the project whose boards are tried first, and
 * how the "no board" sentence names where the ticket lives ("of
 * example.atlassian.net"). */
export interface TicketToOpen {
  readonly backend: BackendKey;
  readonly connection: string;
  readonly issue: string;
  readonly key: string;
  readonly title: string;
  readonly project: string | null;
  readonly of: string;
}

export function computerName(backend: BackendKey | null, fallback: string): string {
  const entry = backend === null ? undefined : attachedBackendEntry(backend);
  return entry ? backendDisplayName(entry) : fallback;
}

/** Opens the ticket in the Issues pane, beside the opener on the desktop,
 * over a board of the ticket's connection. A failed load of the pane's
 * code, a pane that cannot open and a computer with no board of that
 * connection say why in a toast. */
export async function openTicketInIssues(t: TicketToOpen): Promise<void> {
  let requestTicketOpen: typeof import('./issuesPane.svelte').requestTicketOpen;
  try {
    ({ requestTicketOpen } = await loadIssuesPane());
  } catch (err) {
    addErrorToast(`${t.key} cannot open: the Issues pane did not load: ${userFacingError(err)}`, err);
    return;
  }
  // The pane opens first, so a refused open leaves no request for the next
  // Issues pane to take; a pane that mounts after this takes it then.
  try {
    openTool('issues', { newPane: true });
  } catch (err) {
    addErrorToast(`${t.key} cannot open: ${userFacingError(err)}`, err);
    return;
  }
  const computer = computerName(t.backend, 'The ticket\'s computer');
  requestTicketOpen({
    ref: { backend: t.backend, connection: t.connection, issue: t.issue },
    seed: { key: t.key, title: t.title, card: null },
    project: t.project,
    unplaced: (failure) => {
      if (failure) {
        addToast('error', `${computer} could not read its boards, so ${t.key} cannot open: ${failure.report.reason}`, undefined, undefined, failure);
        return;
      }
      addToast('error', `${computer} lists no board ${t.of} in any project, so ${t.key} cannot open. Add one in Settings > Issues > Boards.`);
    },
  });
}
