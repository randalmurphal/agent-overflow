// Every write the ticket screen makes, and the reads its pickers make
// (docs/architecture/tracker-backend.md, "Writes"). Each write goes through
// its bound method once: one write may be pending per ticket and target
// (a field, the status, the points, a comment, the new comment), and a
// second press of a pending target sends nothing. Nothing retries on its
// own; a refusal for what a rich text edit would lose offers Save anyway,
// the same write accepting the loss, as the person's second action. That
// consent is for the one text refused: changing the text or canceling
// withdraws it, so a revised text is checked again.
//
// The text a person is writing (a new comment, a comment's edit, a long
// text field's edit, each with the text it was edited from) is held here
// too, by ticket and target, so it outlives the editor showing it: a
// switch to History, a dropped connection, a linked ticket opened and Back.
// A draft goes with Cancel, an applied save of its target, or its ticket
// leaving every pane's open ticket and Back trail (`holdTicketDrafts`):
// Close, another board, the trail's cap.
//
// A write's outcome is held here under its ticket and target, not in the
// component, so a person who closed the ticket while it ran still learns
// how it ended: a refused or unknown outcome then becomes a toast. An
// applied write stops saying "Saving" once the ticket's view holds what
// its computer last told (`trackerTicketSettled`): the write's refresh
// tells its event before the answer arrives, so the view it published is
// read by then, or the read failed. The view changes only from the
// tracker's answer; nothing is written over it before.
//
// What each outcome shows is in `trackerTicketModel.ts#writeOutcome`.
//
// Files are attached one at a time per ticket, in the order given, each
// its own write under an `attach:` target so its outcome stays beside the
// next file's; the queue says which file is being sent and its place, and
// nothing else (no byte progress). A link added, a link removed and the
// poker writes are writes like any other; a vote, a reveal and a clear
// share one target. A connection's link types are read once, for the link
// form, and held until its computer connects again.

import { SvelteMap } from 'svelte/reactivity';
import type {
  ChoiceListView,
  FieldEditInput,
  LinkTypeView,
  LinkView,
  UserListView,
  WriteView,
} from '../../../bindings/agent-overflow/internal/trackerapp/models';
import {
  AddTrackerComment,
  ClearTrackerPokerVotes,
  DeleteTrackerComment,
  EditTrackerComment,
  EditTrackerIssue,
  LinkTrackerIssues,
  MoveTrackerIssue,
  RevealTrackerPoker,
  SetTrackerEstimate,
  TrackerAssignableUsers,
  TrackerChoices,
  TrackerLinkTypes,
  UnlinkTrackerIssues,
  VoteTrackerPoker,
} from './bindings';
import { attachedBackendEntry, backendDisplayName } from './attachedBackends.svelte';
import { addErrorToast, addToast } from './toast.svelte';
import {
  isTrackerTicketHeld,
  onTrackerTicketClosed,
  refreshTrackerTicket,
  trackerTicketSettled,
  type CommentChange,
  type TrackerTicketRef,
} from './trackerTicket.svelte';
import { linkRequest, lostWriteSentence, refusalSentence, writeOutcome, type LinkRelation, type WriteOutcome } from './trackerTicketModel';
import { onBackendStatusChange } from './transportStatus.svelte';
import { TransferInterruptedError, TransferRefusedError, uploadTrackerAttachmentBytes } from '../transport/attachmentTransfer';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { DisconnectedError } from '../transport/wsClient';
import { composeWorkspaceKey, workspaceKeyBackend } from '../utils/workspaceKey';

/** The ticket a write is made to, as the screen holds it. */
export interface TicketWriteContext {
  /** trackerTicketKey of `ref`. */
  readonly key: string;
  readonly ref: TrackerTicketRef;
  /** The ticket's key as a person reads it, for a toast after it closed. */
  readonly label: string;
}

/** A target's last write. */
export interface TicketWrite {
  readonly pending: boolean;
  /** How it ended; null while pending and once a plain success settled. */
  readonly outcome: WriteOutcome | null;
  /** On a refusal for what a rich text edit would lose: sends the same
   * write accepting the loss. */
  readonly saveAnyway: (() => Promise<WriteOutcome | null>) | null;
  /** What it sends as the person names it (a file's name); null when its
   * target says. */
  readonly subject: string | null;
}

const writes = new SvelteMap<string, TicketWrite>();

/** Text a person is writing, and the text it was edited from (empty for
 * a new comment). */
export interface TicketDraft {
  readonly ticket: string;
  readonly text: string;
  readonly from: string;
}

const drafts = new SvelteMap<string, TicketDraft>();
// The tickets each pane holds open or on its Back trail, by holder.
const draftHolders = new Map<number, ReadonlySet<string>>();
let nextDraftHolder = 0;

function writeKey(ticket: string, target: string): string {
  return `${ticket}\n${target}`;
}

/** A field's target; `status` and `points` are the move's and the
 * estimate's, `new-comment` the comment box's. */
export function fieldTarget(field: string): string {
  return `field:${field}`;
}

export function commentTarget(comment: string): string {
  return `comment:${comment}`;
}

/** The target's last write; reactive. */
export function ticketWrite(ticket: string, target: string): TicketWrite | null {
  return writes.get(writeKey(ticket, target)) ?? null;
}

/** Drops a settled outcome the person read. */
export function dismissTicketWrite(ticket: string, target: string): void {
  const k = writeKey(ticket, target);
  if (writes.get(k)?.pending === false) writes.delete(k);
}

/** Withdraws a Save anyway offer: the text it was for changed or its
 * edit was canceled. Other outcomes stay, a lost answer among them. */
function withdrawLossConsent(k: string): void {
  if (writes.get(k)?.saveAnyway) writes.delete(k);
}

/** The text being written to the target; reactive. */
export function ticketDraft(ticket: string, target: string): TicketDraft | null {
  return drafts.get(writeKey(ticket, target)) ?? null;
}

/** Opens or changes the text being written to the target, edited from
 * `from`; a Save anyway offered for the text before goes. */
export function setTicketDraft(ticket: string, target: string, text: string, from: string): void {
  const k = writeKey(ticket, target);
  withdrawLossConsent(k);
  drafts.set(k, { ticket, text, from });
}

/** Cancel: the text goes, with a Save anyway offered for it. */
export function dropTicketDraft(ticket: string, target: string): void {
  const k = writeKey(ticket, target);
  withdrawLossConsent(k);
  drafts.delete(k);
}

/** One pane's hold on its tickets' drafts: `set` names the tickets it
 * holds open or on its Back trail, and a draft of a ticket no pane holds
 * goes. */
export function holdTicketDrafts(): { set(tickets: readonly string[]): void; release(): void } {
  const id = nextDraftHolder++;
  draftHolders.set(id, new Set());
  const prune = () => {
    for (const [k, d] of drafts) {
      if (![...draftHolders.values()].some((held) => held.has(d.ticket))) drafts.delete(k);
    }
  };
  return {
    set(tickets) {
      draftHolders.set(id, new Set(tickets));
      prune();
    },
    release() {
      draftHolders.delete(id);
      prune();
    },
  };
}

// A ticket's outcomes go with its last surface; a write still pending
// settles into a toast.
onTrackerTicketClosed((ticket) => {
  const prefix = `${ticket}\n`;
  for (const [k, w] of writes) if (k.startsWith(prefix) && !w.pending) writes.delete(k);
});

function computerName(backend: string): string {
  const entry = attachedBackendEntry(backend);
  return entry ? backendDisplayName(entry) : 'The ticket\'s computer';
}

interface WriteOptions {
  /** The comment an applied write changed (`trackerTicketSettled`). */
  readonly comment?: CommentChange;
  /** What a failure of `send` means, in place of a call's rule, when
   * `send` is more than one call (an upload's mint and transfer). */
  readonly failure?: (err: unknown) => WriteOutcome;
  /** What the write sends, as the person names it (a file's name). */
  readonly subject?: string;
}

async function write(
  t: TicketWriteContext,
  target: string,
  send: (acceptLoss: boolean) => Promise<WriteView>,
  acceptLoss: boolean,
  options: WriteOptions = {},
): Promise<WriteOutcome | null> {
  const k = writeKey(t.key, target);
  if (writes.get(k)?.pending) return null;
  const subject = options.subject ?? null;
  writes.set(k, { pending: true, outcome: null, saveAnyway: null, subject });
  let outcome: WriteOutcome;
  try {
    const view = await withBackendTarget(t.ref.backend, () => send(acceptLoss));
    outcome = writeOutcome(view);
  } catch (err) {
    outcome = options.failure?.(err)
      ?? (err instanceof DisconnectedError
        ? { kind: 'lost', message: lostWriteSentence(computerName(t.ref.backend)) }
        : { kind: 'failed', error: err });
  }
  const { comment } = options;
  if (outcome.kind === 'applied') await trackerTicketSettled(t.key, comment);
  // The ticket changed since it was read: read it again, so the person
  // sees what the tracker holds before trying again.
  if (outcome.kind === 'refused' && outcome.refusal.reason === 'stale') void refreshTrackerTicket(t.key);
  const loss = outcome.kind === 'refused' && outcome.refusal.reason === 'loss' && !acceptLoss;
  if (outcome.kind === 'applied') drafts.delete(k);
  if (!isTrackerTicketHeld(t.key)) {
    writes.delete(k);
    toastAfterClose(t, outcome);
    return outcome;
  }
  const plain = outcome.kind === 'applied' && outcome.note === null;
  if (plain) writes.delete(k);
  else writes.set(k, { pending: false, outcome, saveAnyway: loss ? () => write(t, target, send, true, options) : null, subject });
  return outcome;
}

/** A write that settled after its ticket closed: said in a toast unless
 * it was made. */
function toastAfterClose(t: TicketWriteContext, outcome: WriteOutcome): void {
  switch (outcome.kind) {
    case 'applied':
      return;
    case 'refused':
      addToast('error', `The tracker refused the change to ${t.label}: ${refusalSentence(outcome.refusal, (id) => id)}`);
      return;
    case 'unknown':
    case 'not_sent':
    case 'lost':
      addToast('error', `The change to ${t.label}: ${outcome.message}`);
      return;
    case 'failed':
      addErrorToast(`The change to ${t.label} was not confirmed`, outcome.error);
  }
}

/** Sets fields of the ticket (EditTrackerIssue), `target` being the
 * control's. */
export function editTicketFields(t: TicketWriteContext, target: string, fields: FieldEditInput[]): Promise<WriteOutcome | null> {
  return write(t, target, (acceptLoss) => EditTrackerIssue(t.ref.connection, { issue: t.ref.issue, fields, accept_loss: acceptLoss }), false);
}

/** Moves the ticket by one of its moves (MoveTrackerIssue). */
export function moveTicket(t: TicketWriteContext, move: string, fields?: FieldEditInput[]): Promise<WriteOutcome | null> {
  return write(t, 'status', (acceptLoss) => MoveTrackerIssue(t.ref.connection, { issue: t.ref.issue, move, fields, accept_loss: acceptLoss }), false);
}

/** Sets or clears the ticket's estimate on a board (SetTrackerEstimate). */
export function setTicketPoints(t: TicketWriteContext, board: string, value: number | null): Promise<WriteOutcome | null> {
  return write(t, 'points', () => SetTrackerEstimate(t.ref.connection, { board, issue: t.ref.issue, value: value ?? undefined }), false);
}

export function addTicketComment(t: TicketWriteContext, markdown: string): Promise<WriteOutcome | null> {
  return write(t, 'new-comment', () => AddTrackerComment(t.ref.connection, { issue: t.ref.issue, markdown }), false);
}

/** Replaces a comment's body; `from` is the body it was edited from. */
export function editTicketComment(t: TicketWriteContext, comment: string, markdown: string, from: string): Promise<WriteOutcome | null> {
  return write(t, commentTarget(comment), (acceptLoss) => EditTrackerComment(t.ref.connection, { issue: t.ref.issue, comment, markdown, from, accept_loss: acceptLoss }), false, { comment: { id: comment, deleted: false } });
}

export function deleteTicketComment(t: TicketWriteContext, comment: string): Promise<WriteOutcome | null> {
  return write(t, commentTarget(comment), () => DeleteTrackerComment(t.ref.connection, { issue: t.ref.issue, comment }), false, { comment: { id: comment, deleted: true } });
}

/** Links the ticket to `other` (an issue ID) as `relation` reads. */
export function linkTicket(t: TicketWriteContext, relation: LinkRelation, other: string): Promise<WriteOutcome | null> {
  return write(t, 'new-link', () => LinkTrackerIssues(t.ref.connection, linkRequest(relation, t.ref.issue, other)), false);
}

/** The target of a link's removal. */
export function linkTarget(link: string): string {
  return `link:${link}`;
}

/** Removes one of the ticket's links. */
export function unlinkTicket(t: TicketWriteContext, link: LinkView): Promise<WriteOutcome | null> {
  return write(t, linkTarget(link.id), () => UnlinkTrackerIssues(t.ref.connection, { issue: t.ref.issue, link: link.id, other: link.issue.id }), false);
}

/** Casts or changes the person's vote. A vote, a reveal and a clear share
 * the `poker` target, so they never overlap. */
export function voteTicketPoker(t: TicketWriteContext, value: number): Promise<WriteOutcome | null> {
  return write(t, 'poker', () => VoteTrackerPoker(t.ref.connection, { issue: t.ref.issue, vote: value }), false);
}

export function revealTicketPoker(t: TicketWriteContext): Promise<WriteOutcome | null> {
  return write(t, 'poker', () => RevealTrackerPoker(t.ref.connection, t.ref.issue), false);
}

export function clearTicketPokerVotes(t: TicketWriteContext): Promise<WriteOutcome | null> {
  return write(t, 'poker', () => ClearTrackerPokerVotes(t.ref.connection, t.ref.issue), false);
}

/** A ticket's files waiting to be attached and the one being sent; null
 * when none is. */
export interface TicketUploads {
  /** The file being sent. */
  readonly name: string;
  readonly size: number;
  /** Its place among the files given since the queue last emptied, from 1. */
  readonly index: number;
  readonly total: number;
}

interface UploadQueue {
  readonly t: TicketWriteContext;
  readonly files: File[];
  /** Files sent or being sent since the queue last emptied. */
  sent: number;
}

const uploadQueues = new Map<string, UploadQueue>();
const uploads = new SvelteMap<string, TicketUploads>();
let nextUpload = 0;

/** The ticket's upload in progress; reactive. */
export function ticketUploads(ticket: string): TicketUploads | null {
  return uploads.get(ticket) ?? null;
}

/** The target prefix of each attached file's write: one per file, so an
 * outcome stays beside the next file's. */
export const ATTACH_TARGET_PREFIX = 'attach:';

/** The settled outcomes of the ticket's attached files that are still
 * shown (a file attached plainly leaves none), oldest first; reactive. */
export function ticketAttachOutcomes(ticket: string): readonly { readonly target: string; readonly name: string; readonly outcome: WriteOutcome }[] {
  const prefix = writeKey(ticket, ATTACH_TARGET_PREFIX);
  const out: { target: string; name: string; outcome: WriteOutcome }[] = [];
  for (const [k, w] of writes) {
    if (!k.startsWith(prefix) || !w.outcome) continue;
    out.push({ target: k.slice(ticket.length + 1), name: w.subject ?? '', outcome: w.outcome });
  }
  return out;
}

/** Attaches files to the ticket one at a time, in the order given, after
 * any already waiting. Each is minted just before it is sent, so a file
 * the tracker cannot take is refused before its bytes cross and the next
 * one still goes. Nothing is sent again on its own. */
export function attachTicketFiles(t: TicketWriteContext, files: readonly File[]): void {
  if (files.length === 0) return;
  const queue = uploadQueues.get(t.key);
  if (queue) {
    queue.files.push(...files);
    const shown = uploads.get(t.key);
    if (shown) uploads.set(t.key, { ...shown, total: queue.sent + queue.files.length });
    return;
  }
  const fresh: UploadQueue = { t, files: [...files], sent: 0 };
  uploadQueues.set(t.key, fresh);
  void drainUploads(fresh);
}

async function drainUploads(queue: UploadQueue): Promise<void> {
  const { t } = queue;
  try {
    for (let file = queue.files.shift(); file; file = queue.files.shift()) {
      queue.sent += 1;
      uploads.set(t.key, { name: file.name, size: file.size, index: queue.sent, total: queue.sent + queue.files.length });
      const target = `${ATTACH_TARGET_PREFIX}${nextUpload++}`;
      const sending = file;
      await write(t, target, () => uploadTrackerAttachmentBytes(t.ref.backend, t.ref.connection, t.ref.issue, sending), false, { failure: uploadFailure, subject: file.name });
    }
  } finally {
    uploadQueues.delete(t.key);
    uploads.delete(t.key);
  }
}

/** What a failed upload means. A refusal the route worded before the
 * tracker judged the file attached nothing. A transfer that broke off, or
 * a failure the route did not word, may or may not have attached it. A
 * failed mint, its connection's drop included, sent no byte: a failed
 * call, whose error says why. */
function uploadFailure(err: unknown): WriteOutcome {
  if (err instanceof TransferRefusedError && err.status < 500) return { kind: 'not_sent', message: err.message };
  if (err instanceof TransferRefusedError || err instanceof TransferInterruptedError) return { kind: 'unknown', message: UPLOAD_UNKNOWN };
  return { kind: 'failed', error: err };
}

const UPLOAD_UNKNOWN = 'The file may or may not be attached; look at the ticket\'s attachments before attaching it again.';

/** A connection's link types as last read; reactive. */
export interface TicketLinkTypes {
  readonly types: readonly LinkTypeView[] | null;
  readonly loading: boolean;
  readonly error: unknown;
}

const linkTypes = new SvelteMap<string, TicketLinkTypes>();

/** The connection's link types; null before `loadTicketLinkTypes`. */
export function ticketLinkTypes(backend: BackendKey, connection: string): TicketLinkTypes | null {
  return linkTypes.get(composeWorkspaceKey(backend, connection)) ?? null;
}

/** Reads the connection's link types unless they are held or being read,
 * or read again after a failure. Held until the computer connects again. */
export async function loadTicketLinkTypes(backend: BackendKey, connection: string): Promise<void> {
  const key = composeWorkspaceKey(backend, connection);
  const held = linkTypes.get(key);
  if (held && (held.loading || held.types)) return;
  linkTypes.set(key, { types: null, loading: true, error: null });
  try {
    const types = await withBackendTarget(backend, () => TrackerLinkTypes(connection));
    if (linkTypes.get(key)?.loading) linkTypes.set(key, { types, loading: false, error: null });
  } catch (err) {
    if (linkTypes.get(key)?.loading) linkTypes.set(key, { types: null, loading: false, error: err });
  }
}

// A computer that connects again may hold other link types.
onBackendStatusChange((backend, status) => {
  if (status.status !== 'connected') return;
  for (const key of [...linkTypes.keys()]) if (workspaceKeyBackend(key) === backend) linkTypes.delete(key);
});

/** The people who may be assigned the ticket whose name starts with
 * `text`. */
export function assignableUsers(t: TicketWriteContext, text: string): Promise<UserListView> {
  return withBackendTarget(t.ref.backend, () => TrackerAssignableUsers(t.ref.connection, { issue: t.ref.issue }, text));
}

/** The values `field` allows on the ticket whose name starts with `text`:
 * on the ticket, or on the screen of its move `move`, whose values can
 * differ (MoveView.fields). */
export function fieldChoices(t: TicketWriteContext, field: string, text: string, move?: string): Promise<ChoiceListView> {
  const scope = move ? { issue: t.ref.issue, move } : { issue: t.ref.issue };
  return withBackendTarget(t.ref.backend, () => TrackerChoices(t.ref.connection, scope, field, text));
}

export function resetTicketWritesForTest(): void {
  writes.clear();
  drafts.clear();
  draftHolders.clear();
  uploadQueues.clear();
  uploads.clear();
  linkTypes.clear();
}
