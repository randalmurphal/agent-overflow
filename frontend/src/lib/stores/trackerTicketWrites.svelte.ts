// Every write the ticket screen makes, and the reads its pickers make
// (docs/architecture/tracker-backend.md, "Writes"). Each write goes through
// its bound method once: one write may be pending per ticket and target
// (a field, the status, the points, a comment, the new comment), and a
// second press of a pending target sends nothing. Nothing retries on its
// own; a refusal for what a rich text edit would lose offers Save anyway,
// the same write accepting the loss, as the person's second action. That
// consent is for the one text refused: the editor withdraws it when the
// person changes the text or cancels, so a revised text is checked again.
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
export function withdrawTicketLossConsent(ticket: string, target: string): void {
  const k = writeKey(ticket, target);
  if (writes.get(k)?.saveAnyway) writes.delete(k);
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
  if (outcome.kind === 'applied') await trackerTicketSettled(t.key);
  // The ticket changed since it was read: read it again, so the person
  // sees what the tracker holds before trying again.
  if (outcome.kind === 'refused' && outcome.refusal.reason === 'stale') void refreshTrackerTicket(t.key);
  const loss = outcome.kind === 'refused' && outcome.refusal.reason === 'loss' && !acceptLoss;
  if (!isTrackerTicketHeld(t.key)) {
    writes.delete(k);
    toastAfterClose(t, outcome);
    return outcome;
  }
  const plain = outcome.kind === 'applied' && outcome.note === null;
  if (plain) writes.delete(k);
  else writes.set(k, { pending: false, outcome, saveAnyway: loss ? () => write(t, target, send, true) : null });
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
  return write(t, commentTarget(comment), (acceptLoss) => EditTrackerComment(t.ref.connection, { issue: t.ref.issue, comment, markdown, from, accept_loss: acceptLoss }), false);
}

export function deleteTicketComment(t: TicketWriteContext, comment: string): Promise<WriteOutcome | null> {
  return write(t, commentTarget(comment), () => DeleteTrackerComment(t.ref.connection, { issue: t.ref.issue, comment }), false);
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
}
