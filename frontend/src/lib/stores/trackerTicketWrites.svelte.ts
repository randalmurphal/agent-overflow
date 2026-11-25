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

import { SvelteMap } from 'svelte/reactivity';
import type {
  ChoiceListView,
  FieldEditInput,
  UserListView,
  WriteView,
} from '../../../bindings/agent-overflow/internal/trackerapp/models';
import {
  AddTrackerComment,
  DeleteTrackerComment,
  EditTrackerComment,
  EditTrackerIssue,
  MoveTrackerIssue,
  SetTrackerEstimate,
  TrackerAssignableUsers,
  TrackerChoices,
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
import { lostWriteSentence, refusalSentence, writeOutcome, type WriteOutcome } from './trackerTicketModel';
import { withBackendTarget } from '../transport/backends';
import { DisconnectedError } from '../transport/wsClient';

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

async function write(
  t: TicketWriteContext,
  target: string,
  send: (acceptLoss: boolean) => Promise<WriteView>,
  acceptLoss: boolean,
  comment?: CommentChange,
): Promise<WriteOutcome | null> {
  const k = writeKey(t.key, target);
  if (writes.get(k)?.pending) return null;
  writes.set(k, { pending: true, outcome: null, saveAnyway: null });
  let outcome: WriteOutcome;
  try {
    const view = await withBackendTarget(t.ref.backend, () => send(acceptLoss));
    outcome = writeOutcome(view);
  } catch (err) {
    outcome = err instanceof DisconnectedError
      ? { kind: 'lost', message: lostWriteSentence(computerName(t.ref.backend)) }
      : { kind: 'failed', error: err };
  }
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
  else writes.set(k, { pending: false, outcome, saveAnyway: loss ? () => write(t, target, send, true, comment) : null });
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
  return write(t, commentTarget(comment), (acceptLoss) => EditTrackerComment(t.ref.connection, { issue: t.ref.issue, comment, markdown, from, accept_loss: acceptLoss }), false, { id: comment, deleted: false });
}

export function deleteTicketComment(t: TicketWriteContext, comment: string): Promise<WriteOutcome | null> {
  return write(t, commentTarget(comment), () => DeleteTrackerComment(t.ref.connection, { issue: t.ref.issue, comment }), false, { id: comment, deleted: true });
}

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
}
