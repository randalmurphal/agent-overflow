// A thread's ticket link as the screens act on it (docs/specs/issues.md,
// "Thread link"): open the ticket in the Issues pane, copy its key, remove
// the link. The thread header's button, its menu and the sidebar row's
// menu call these and nothing else, so each action has one behavior on
// every surface. The link itself is the thread row's (`Thread.ticketLink`),
// which the thread's computer derives and pushes; nothing here holds it.
//
// The Issues pane's stores load with the pane, so the request that tells
// it which ticket to open is reached through one stable dynamic import
// (architecture.test.ts, rule 4b).

import { ClearThreadTicket } from './bindings';
import { attachedBackendEntry, backendDisplayName, threadMachine } from './attachedBackends.svelte';
import { syncThreadReply } from './panes.svelte';
import { threadRowFrameMark } from './threadRowFrames';
import { addErrorToast, addToast } from './toast.svelte';
import { openTool } from './toolPanes.svelte';
import { isEmptyDraftCleanupPending, settleEmptyDraftCleanup } from './emptyDraftCleanup';
import { copyToClipboard } from '../utils/clipboard';
import { userFacingError } from '../utils/userFacingError';
import type { Thread } from '../types/models';
import type { ThreadTicketLink } from '../../../bindings/agent-overflow/internal/store/models';

let issuesPaneModule: Promise<typeof import('./issuesPane.svelte')> | null = null;
function loadIssuesPane(): Promise<typeof import('./issuesPane.svelte')> {
  issuesPaneModule ??= import('./issuesPane.svelte');
  return issuesPaneModule;
}

function trackerName(kind: string): string {
  return kind === 'linear' ? 'Linear' : 'Jira';
}

/** A site as a person names it: a Jira site without its scheme, a Linear
 * workspace's key as it is. */
function siteName(site: string): string {
  return site.replace(/^https:\/\//, '');
}

function computerName(thread: Thread): string {
  const entry = attachedBackendEntry(threadMachine(thread.id, thread.projectId));
  return entry ? backendDisplayName(entry) : 'The thread\'s computer';
}

/** Why the thread's computer cannot open its linked ticket, as a sentence;
 * null when it can: it reads the ticket through a connection that is on. */
export function linkedTicketUnopenable(thread: Thread, link: ThreadTicketLink): string | null {
  if (link.connectionId) return null;
  const tracker = trackerName(link.kind);
  const site = siteName(link.site);
  if (link.state === 'turned_off') {
    return `${computerName(thread)} has its ${tracker} connection to ${site} turned off, so ${link.key} cannot open. Turn it on in Settings > Issues > Connections.`;
  }
  return `${computerName(thread)} has no ${tracker} connection to ${site}, so ${link.key} cannot open. Add one in Settings > Issues > Connections.`;
}

/** What the link's mark and title say of its state, beside the key and
 * title: empty for a ticket read and current. */
export function linkedTicketStateNote(link: ThreadTicketLink): string {
  switch (link.state) {
    case 'not_found':
      return `The last read of ${trackerName(link.kind)} did not return it: it was deleted, or the token's user may not see it.`;
    case 'not_checked':
      return 'Not read on this computer yet.';
    case 'turned_off':
      return `The ${trackerName(link.kind)} connection is turned off; shown as last read.`;
    case 'no_connection':
      return `No ${trackerName(link.kind)} connection to ${siteName(link.site)} on its computer; shown as last read.`;
  }
  return '';
}

/**
 * Opens the thread's linked ticket in the Issues pane, beside the thread on
 * the desktop, over a board of the ticket's connection (the thread's
 * project's first). A ticket its computer cannot read, a page that may not
 * open Issues, and a computer with no board of that connection say why in
 * a toast.
 */
export async function openLinkedTicket(thread: Thread): Promise<void> {
  const link = thread.ticketLink;
  if (!link) return;
  const refusal = linkedTicketUnopenable(thread, link);
  if (refusal) {
    addToast('error', refusal);
    return;
  }
  const backend = threadMachine(thread.id, thread.projectId);
  const { requestTicketOpen } = await loadIssuesPane();
  // The pane opens first, so a refused open leaves no request for the next
  // Issues pane to take; a pane that mounts after this takes it then.
  try {
    openTool('issues', { newPane: true });
  } catch (err) {
    addErrorToast(`${link.key} cannot open: ${userFacingError(err)}`, err);
    return;
  }
  requestTicketOpen({
    ref: { backend, connection: link.connectionId, issue: link.issueId },
    seed: { key: link.key, title: link.title, card: null },
    project: thread.projectId ?? null,
    unplaced: (failure) => {
      if (failure) {
        addToast('error', `${computerName(thread)} could not read its boards, so ${link.key} cannot open: ${failure.report.reason}`, undefined, undefined, failure);
        return;
      }
      addToast('error', `${computerName(thread)} lists no board of ${siteName(link.site)} in any project, so ${link.key} cannot open. Add one in Settings > Issues > Boards.`);
    },
  });
}

/** Copies the linked ticket's key. */
export async function copyLinkedTicketKey(thread: Thread): Promise<void> {
  const key = thread.ticketLink?.key;
  if (key && !(await copyToClipboard(key))) addToast('error', `Could not copy ${key}.`);
}

/** Removes the thread's link; the row the computer answers is applied, and
 * a refusal is a toast with the computer's sentence. */
export async function removeThreadTicketLink(thread: Thread): Promise<void> {
  try {
    if (isEmptyDraftCleanupPending(thread.id) && await settleEmptyDraftCleanup(thread.id)) return;
    const mark = threadRowFrameMark();
    const row = (await ClearThreadTicket(thread.id)) as Thread;
    syncThreadReply(row, mark);
  } catch (err) {
    addErrorToast(`The link to ${thread.ticketLink?.key ?? 'the ticket'} was not removed: ${userFacingError(err)}`, err);
  }
}
