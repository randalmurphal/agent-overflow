// Opening a ticket in the Issues pane from outside it: a thread's linked
// ticket, a pull request's chip; and the Time screen at a week, from a
// thread's time draft card. The Issues pane's stores load with the
// pane, so the request that tells it which ticket to open is reached
// through one stable dynamic import (architecture.test.ts, rule 4b).

import { attachedBackendEntry, backendDisplayName } from './attachedBackends.svelte';
import { TrackerLookup } from './bindings';
import { addErrorToast, addToast } from './toast.svelte';
import { withBackendTarget } from '../transport/backends';
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

/** A ticket a pull request names (PRListItem.tickets): its key and
 * connection, with its issue and title when the computer's mirror holds
 * them. */
export interface NamedTicket {
  readonly key: string;
  readonly connectionId: string;
  readonly issueId?: string;
  readonly title?: string;
}

/** Opens a ticket a pull request names, on `backend`; a key the mirror
 * did not hold is looked up on the tracker first. */
export async function openNamedTicket(backend: BackendKey, ticket: NamedTicket, project: string | null): Promise<void> {
  let issue = ticket.issueId ?? '';
  let title = ticket.title ?? '';
  if (!issue) {
    try {
      const view = await withBackendTarget(backend, () => TrackerLookup(ticket.connectionId, [ticket.key]));
      const card = view.found?.find((m) => m.key === ticket.key)?.card;
      if (!card) {
        addToast('error', `${ticket.key} cannot open: its tracker has no ticket with that key.`);
        return;
      }
      issue = card.id;
      title = card.title;
    } catch (err) {
      addErrorToast(`${ticket.key} cannot open: ${userFacingError(err)}`, err);
      return;
    }
  }
  await openTicketInIssues({ backend, connection: ticket.connectionId, issue, key: ticket.key, title, project, of: 'of its tracker' });
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

/** A week to show on the Time screen: its computer, the connection whose
 * first board it opens over, and a day of it. */
export interface TimeToOpen {
  readonly backend: BackendKey;
  readonly connection: string;
  readonly day: string;
}

/** Opens the Issues pane on the Time screen at the week of `t.day`, over
 * the first board of the connection on that computer. A failed load, a
 * pane that cannot open and a computer with no board of that connection
 * say why in a toast. */
export async function openTimeInIssues(t: TimeToOpen): Promise<void> {
  let requestTimeOpen: typeof import('./issuesPane.svelte').requestTimeOpen;
  try {
    ({ requestTimeOpen } = await loadIssuesPane());
  } catch (err) {
    addErrorToast(`The draft cannot open in Time: the Issues pane did not load: ${userFacingError(err)}`, err);
    return;
  }
  try {
    openTool('issues', { newPane: true });
  } catch (err) {
    addErrorToast(`The draft cannot open in Time: ${userFacingError(err)}`, err);
    return;
  }
  const computer = computerName(t.backend, 'The draft\'s computer');
  requestTimeOpen({
    time: { backend: t.backend, connection: t.connection, day: t.day },
    project: null,
    unplaced: (failure) => {
      if (failure) {
        addToast('error', `${computer} could not read its boards, so the draft cannot open in Time: ${failure.report.reason}`, undefined, undefined, failure);
        return;
      }
      addToast('error', `${computer} lists no board of this draft's connection in any project, so the draft cannot open in Time. Add one in Settings > Issues > Boards.`);
    },
  });
}
