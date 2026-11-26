// A thread's ticket link as the screens act on it (docs/specs/issues.md,
// "Thread link"): open the ticket in the Issues pane, copy its key, link
// or change it through the ticket finder, remove it. The thread header's
// button, its menu and the sidebar row's menu call these and nothing
// else, so each action has one behavior on every surface. The link itself
// is the thread row's (`Thread.ticketLink`), which the thread's computer
// derives and pushes; nothing here holds it. What is held here is the one
// finder open, for the thread it links (`ThreadTicketFinderHost`).
//
// The ticket opens through the one opener of a ticket from outside the
// Issues pane (`ticketOpen.ts`).
//
// Every read and open of the thread's computer goes to the computer the
// thread verifiably runs on (`verifiedThreadMachine`): its tracker
// credentials are spent there, and a thread two computers claim, or one
// whose owner is unknown while several are attached, is said in a toast.

import { ClearThreadTicket, SetThreadTicket } from './bindings';
import { trackerKindName } from '../components/settings/trackerConnectionStatus';
import { verifiedThreadMachine } from './attachedBackends.svelte';
import { syncThreadReply } from './panes.svelte';
import { threadRowFrameMark } from './threadRowFrames';
import { addErrorToast, addToast } from './toast.svelte';
import { computerName as computerCalled, openTicketInIssues } from './ticketOpen';
import { isEmptyDraftCleanupPending, settleEmptyDraftCleanup } from './emptyDraftCleanup';
import { threadScopeGrant } from '../transport/entityScopes';
import type { EntityScopeGrant } from '../transport/scopes';
import { copyToClipboard } from '../utils/clipboard';
import { userFacingError } from '../utils/userFacingError';
import { SvelteMap } from 'svelte/reactivity';
import { isDraftPlaceholderId } from './draftPlaceholderId';
import type { BackendKey } from '../transport/backendKey';
import type { ThreadPane } from './thread.svelte';
import type { Thread } from '../types/models';
import type { ThreadTicketLink } from '../../../bindings/agent-overflow/internal/store/models';

/** What a thread no computer verifiably holds says, after "so". */
const UNOWNED = 'Agent Overflow cannot tell which computer holds this thread';

/** A site as a person names it: a Jira site without its scheme, a Linear
 * workspace's key as it is. */
function siteName(site: string): string {
  return site.replace(/^https:\/\//, '');
}

function computerName(backend: BackendKey | null): string {
  return computerCalled(backend, 'The thread\'s computer');
}

/** Why the thread's computer cannot open its linked ticket, as a sentence;
 * null when it can: it reads the ticket through a connection that is on. */
export function linkedTicketUnopenable(thread: Thread, link: ThreadTicketLink): string | null {
  if (link.connectionId) return null;
  const computer = computerName(verifiedThreadMachine(thread.id));
  const tracker = trackerKindName(link.kind);
  const site = siteName(link.site);
  if (link.state === 'turned_off') {
    return `${computer} has its ${tracker} connection to ${site} turned off, so ${link.key} cannot open. Turn it on in Settings > Issues > Connections.`;
  }
  return `${computer} has no ${tracker} connection to ${site}, so ${link.key} cannot open. Add one in Settings > Issues > Connections.`;
}

/** What the link's mark and title say of its state, beside the key and
 * title: empty for a ticket read and current. */
export function linkedTicketStateNote(link: ThreadTicketLink): string {
  switch (link.state) {
    case 'not_found':
      return `The last read of ${trackerKindName(link.kind)} did not return it: it was deleted, or the token's user may not see it.`;
    case 'not_checked':
      return 'Not read on this computer yet.';
    case 'turned_off':
      return `The ${trackerKindName(link.kind)} connection is turned off; shown as last read.`;
    case 'no_connection':
      return `No ${trackerKindName(link.kind)} connection to ${siteName(link.site)} on its computer; shown as last read.`;
  }
  return '';
}

/**
 * Opens the thread's linked ticket in the Issues pane, beside the thread on
 * the desktop, over a board of the ticket's connection (the thread's
 * project's first). A ticket its computer cannot read, a thread no
 * computer verifiably holds, a page that may not open Issues, a failed load
 * of the pane's code and a computer with no board of that connection say
 * why in a toast.
 */
export async function openLinkedTicket(thread: Thread): Promise<void> {
  const link = thread.ticketLink;
  if (!link) return;
  const refusal = linkedTicketUnopenable(thread, link);
  if (refusal) {
    addToast('error', refusal);
    return;
  }
  const backend = verifiedThreadMachine(thread.id);
  if (backend === null) {
    addToast('error', `${UNOWNED}, so ${link.key} cannot open.`);
    return;
  }
  await openTicketInIssues({
    backend, connection: link.connectionId, issue: link.issueId, key: link.key, title: link.title,
    project: thread.projectId ?? null, of: `of ${siteName(link.site)}`,
  });
}

/** A ticket a thread's agent created or changed, as its timeline row names
 * it (`trackertools.WrittenTicket`). */
export interface WrittenTicket {
  readonly connection: string;
  readonly issue: string;
  readonly key: string;
  readonly title: string;
}

/**
 * Opens a ticket the thread's agent wrote, from its timeline row, on the
 * computer the thread verifiably runs on, over a board of the thread's
 * project first.
 */
export async function openWrittenTicket(threadId: string, projectId: string | null, ticket: WrittenTicket): Promise<void> {
  const backend = verifiedThreadMachine(threadId);
  if (backend === null) {
    addToast('error', `${UNOWNED}, so ${ticket.key} cannot open.`);
    return;
  }
  await openTicketInIssues({
    backend, connection: ticket.connection, issue: ticket.issue, key: ticket.key, title: ticket.title,
    project: projectId, of: 'of its tracker',
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

/** Whether this page may link the thread to a ticket on its computer:
 * SetThreadTicket rides threads:operate and reads the ticket with
 * tracker:read. The first grant that is not given is the answer. */
export function threadTicketLinkGrant(thread: Thread): EntityScopeGrant {
  const operate = threadScopeGrant('threads:operate', thread.id, thread.projectId);
  return operate !== 'granted' ? operate : threadScopeGrant('tracker:read', thread.id, thread.projectId);
}

/** The ticket finder open for a thread, at the control it was opened from. */
export interface ThreadTicketFinding {
  /** The thread shown, which on a New thread is its placeholder. */
  readonly thread: Thread;
  /** The computer the thread verifiably runs on, whose trackers it reads. */
  readonly backend: BackendKey;
  readonly anchor: HTMLElement | undefined;
  /** The pane the finder was opened from, null for the sidebar's row. A
   * New thread's pane creates its thread when a ticket is picked. */
  readonly pane: ThreadPane | null;
}

// The one finder open; a later Link or Change replaces it.
let finding = $state.raw<ThreadTicketFinding | null>(null);

// The panes whose thread a link is being written to, from the pick to the
// row the computer answers: the composer's empty-draft cleanup waits for
// it (ticketLinkPending), since a New thread is materialized, empty, before
// its link lands.
const linkingPanes = new SvelteMap<string, number>();

/** Whether a link is being written to the thread pane `paneId` shows;
 * reactive. */
export function ticketLinkPending(paneId: string): boolean {
  return (linkingPanes.get(paneId) ?? 0) > 0;
}

/** Opens the ticket finder to link `thread` to a ticket of its computer's,
 * or change the ticket it links (Link ticket…, Change ticket…). A New
 * thread passes the `pane` showing it. A thread no computer verifiably
 * holds says so in a toast and opens nothing. */
export function findThreadTicket(thread: Thread, anchor: HTMLElement | undefined, pane: ThreadPane | null = null): void {
  const backend = verifiedThreadMachine(thread.id);
  if (backend === null) {
    addToast('error', `${UNOWNED}, so it cannot be linked to a ticket.`);
    return;
  }
  if (isDraftPlaceholderId(thread.id) && pane?.draftPlaceholder?.id !== thread.id) {
    throw new Error(`findThreadTicket: the New thread ${thread.id} needs the pane that shows it`);
  }
  finding = { thread, backend, anchor, pane };
}

/** The finder open, or null; reactive. */
export function threadTicketFinding(): ThreadTicketFinding | null {
  return finding;
}

export function closeThreadTicketFinder(): void {
  finding = null;
}

/** A ticket as SetThreadTicket names it, with its key for what is said. */
export interface ThreadTicketChoice {
  readonly kind: string;
  readonly site: string;
  readonly issueId: string;
  readonly key: string;
}

/** The thread `found` links, created first when it is a New thread: its
 * pane creates it while the pane still shows that New thread, so a ticket
 * is linked only to a thread with a row, and never to another thread the
 * pane moved to. Null after a refusal said in a toast. */
async function threadToLink(found: ThreadTicketFinding, key: string): Promise<string | null> {
  const { thread, pane } = found;
  if (!pane || !isDraftPlaceholderId(thread.id)) return thread.id;
  if (pane.draftPlaceholder?.id !== thread.id) {
    addToast('error', `The thread was not linked to ${key}: the New thread is no longer open.`);
    return null;
  }
  // The pane says why a create failed (setGeneralError).
  return pane.ensureMaterializedThread();
}

/** Links the thread `found` names to `ticket`, replacing its link; the row
 * the computer answers is applied, and a refusal is a toast with the
 * computer's sentence. */
export async function linkThreadTicket(found: ThreadTicketFinding, ticket: ThreadTicketChoice): Promise<void> {
  const key = ticket.key || 'the ticket';
  const paneId = found.pane?.paneId;
  if (paneId) linkingPanes.set(paneId, (linkingPanes.get(paneId) ?? 0) + 1);
  try {
    const threadId = await threadToLink(found, key);
    if (threadId === null) return;
    if (isEmptyDraftCleanupPending(threadId) && await settleEmptyDraftCleanup(threadId)) return;
    const mark = threadRowFrameMark();
    const row = (await SetThreadTicket(threadId, { kind: ticket.kind, site: ticket.site, issueId: ticket.issueId })) as Thread;
    syncThreadReply(row, mark);
  } catch (err) {
    addErrorToast(`The thread was not linked to ${key}: ${userFacingError(err)}`, err);
  } finally {
    if (paneId) {
      const left = (linkingPanes.get(paneId) ?? 1) - 1;
      if (left > 0) linkingPanes.set(paneId, left);
      else linkingPanes.delete(paneId);
    }
  }
}

export function resetThreadTicketFinderForTest(): void {
  finding = null;
  linkingPanes.clear();
}
