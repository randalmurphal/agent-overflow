// Every write the ticket screen and a Board card make, and the reads
// their pickers make (docs/architecture/tracker-backend.md, "Writes"). A
// card writes under the same ticket key as the ticket screen, so one write
// shows one state on both. Each write goes through
// its bound method once: one write may be pending per ticket and target
// (a field, the status, the points, a comment, the new comment), and a
// second press of a pending target sends nothing. Nothing retries on its
// own; a refusal for what a rich text edit would lose offers Save anyway,
// the same write accepting the loss, as the person's second action.
//
// What a person is writing is held here too, by ticket and target, so it
// outlives the editor showing it (a switch to History, a dropped
// connection, a linked ticket opened and Back): a new comment, a comment's
// edit, a long text field's edit, each with the text it was edited from,
// and a move's form (target `status`), with the text each long field of
// it started from. A draft goes with Cancel, an applied write of its
// target, or its ticket leaving every pane's open ticket and Back trail
// (`holdTicketDrafts`): Close, another board, the trail's cap.
//
// Loss consent is `trackerWriteOutcome.ts#LossConsent`'s, for every write
// a loss can refuse (a section's text, a comment's edit, a move's form):
// every change of a draft here is `putDraft`, or a prune of a ticket no
// pane holds, and each revises the target's consent. A write that is made,
// by Save or by Save anyway, drops its target's draft, which closes the
// editor or form showing it. Every editor and the move's form, its pickers
// included, are disabled while their target's write is pending.
//
// A write's outcome is held here under its ticket and target, not in the
// component. Where it shows. What must hold, for every write:
// - a target is shown while a surface that draws its control holds the
//   ticket (`targetShown`): the ticket screen draws every target, and a
//   held board whose rows include the issue draws the card's targets
//   (`BOARD_CARD_TARGETS`: the status, rank, sprint, type, priority,
//   assignee and points);
// - a write that settles while its target is shown keeps a refused,
//   unknown or noted outcome under that control until the person
//   dismisses it, and a plain success shows nothing;
// - a write that settles while its target is not shown is dropped, and a
//   refused or unknown one becomes a toast, so a person who closed the
//   ticket and the board while it ran still learns how it ended;
// - when the last surface drawing a target lets it go (the ticket's last
//   surface closes, or a board is released), its settled outcomes go; a
//   pending write stays and settles by the rule above.
// So the ticket closing while a board still shows the card keeps the
// card's outcomes, on the card, and drops the rest as before.
//
// An applied write stops saying "Saving" once the ticket's view and every
// held board of its connection hold what their computer last told
// (`trackerTicketSettled`, `trackerBoardsSettled`): the write's refresh
// tells their events before the answer arrives, so what it published is
// read by then, or the read failed. The views change only from the
// tracker's answer; nothing is written over them before. A Board card
// dropped in another place is drawn there while its write is out
// (`trackerBoardCarry.svelte.ts`), which changes nothing the board holds.
//
// The send and what each outcome shows are `trackerWriteOutcome.ts`'s.

import { SvelteMap } from 'svelte/reactivity';
import type {
  ChoiceListView,
  FieldEditInput,
  MoveView,
  UserListView,
  WriteView,
} from '../../../bindings/agent-overflow/internal/trackerapp/models';
import {
  AddTrackerComment,
  DeleteTrackerComment,
  EditTrackerComment,
  EditTrackerIssue,
  MoveTrackerIssue,
  MoveTrackerIssuesToSprint,
  RankTrackerIssues,
  SetTrackerEstimate,
  TrackerAssignableUsers,
  TrackerChoices,
  TrackerMoves,
} from './bindings';
import { attachedBackendEntry, backendDisplayName } from './attachedBackends.svelte';
import { onTrackerBoardDropped, trackerBoardShowsIssue, trackerBoardsSettled } from './trackerBoard.svelte';
import { addErrorToast, addToast } from './toast.svelte';
import {
  isTrackerTicketHeld,
  onTrackerTicketClosed,
  refreshTrackerTicket,
  trackerTicketKey,
  trackerTicketSettled,
  type CommentChange,
  type TrackerTicketRef,
} from './trackerTicket.svelte';
import { LossConsent, refusalSentence, sendWrite, TICKET_SUBJECT, type WriteOutcome } from './trackerWriteOutcome';
import { withBackendTarget } from '../transport/backends';

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

/** A target's write as held: its ticket and target, for the rule of
 * where it shows. */
interface HeldWrite extends TicketWrite {
  readonly ref: TrackerTicketRef;
  readonly target: string;
}

const writes = new SvelteMap<string, HeldWrite>();

/** Text a person is writing, and the text it was edited from (empty for
 * a new comment). */
export interface TicketDraft {
  readonly kind: 'text';
  readonly ticket: string;
  readonly text: string;
  readonly from: string;
}

/** A field of a move's form the person filled: the edit it sends, the
 * values a pick left it holding, and its text. */
export interface MoveFieldDraft {
  readonly edit: FieldEditInput;
  readonly picked: readonly { readonly id: string; readonly name: string }[];
  readonly text: string;
}

/** A move's form: the move, the text each long field started from (its
 * edit base), and the fields filled. */
export interface TicketMoveDraft {
  readonly kind: 'move';
  readonly ticket: string;
  readonly move: MoveView;
  readonly bases: ReadonlyMap<string, string>;
  readonly fields: ReadonlyMap<string, MoveFieldDraft>;
}

type Draft = TicketDraft | TicketMoveDraft;

const drafts = new SvelteMap<string, { readonly draft: Draft }>();
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

/** The targets a Board card draws a control of. */
export const BOARD_CARD_TARGETS: readonly string[] = ['status', 'rank', 'sprint', fieldTarget('type'), fieldTarget('priority'), fieldTarget('assignee'), 'points'];

/** Whether a surface that draws the target's control holds the ticket. */
function targetShown(key: string, ref: TrackerTicketRef, target: string): boolean {
  if (isTrackerTicketHeld(key)) return true;
  return BOARD_CARD_TARGETS.includes(target) && trackerBoardShowsIssue(ref.backend, ref.connection, ref.issue);
}

/** Drops the settled outcomes `which` names that no surface shows now. */
function pruneUnshown(which: (k: string, w: HeldWrite) => boolean): void {
  for (const [k, w] of writes) {
    if (!w.pending && which(k, w) && !targetShown(trackerTicketKey(w.ref), w.ref, w.target)) writes.delete(k);
  }
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

// A withdrawn offer drops its outcome; other outcomes stay, a lost answer
// among them.
const consent = new LossConsent((k) => {
  if (writes.get(k)?.saveAnyway) writes.delete(k);
});

/** The one change of a draft: a new revision, and no Save anyway for the
 * one before. */
function putDraft(k: string, draft: Draft | null): void {
  consent.revise(k);
  if (draft) drafts.set(k, { draft });
  else drafts.delete(k);
}

/** The text being written to the target; reactive. */
export function ticketDraft(ticket: string, target: string): TicketDraft | null {
  const held = drafts.get(writeKey(ticket, target))?.draft;
  return held?.kind === 'text' ? held : null;
}

/** Opens or changes the text being written to the target, edited from
 * `from`. */
export function setTicketDraft(ticket: string, target: string, text: string, from: string): void {
  putDraft(writeKey(ticket, target), { kind: 'text', ticket, text, from });
}

/** Cancel: the draft goes, a text or a move's form. */
export function dropTicketDraft(ticket: string, target: string): void {
  putDraft(writeKey(ticket, target), null);
}

/** The move form open on the ticket; reactive. */
export function ticketMoveDraft(ticket: string): TicketMoveDraft | null {
  const held = drafts.get(writeKey(ticket, 'status'))?.draft;
  return held?.kind === 'move' ? held : null;
}

/** Opens the form of `move`, in place of any open, each long field
 * starting from the text the ticket shows for it (`bases`). */
export function openTicketMove(ticket: string, move: MoveView, bases: ReadonlyMap<string, string>): void {
  putDraft(writeKey(ticket, 'status'), { kind: 'move', ticket, move, bases, fields: new Map() });
}

/** Fills one field of the open move form. */
export function setTicketMoveField(ticket: string, field: string, filled: MoveFieldDraft): void {
  const open = ticketMoveDraft(ticket);
  if (!open) return;
  putDraft(writeKey(ticket, 'status'), { ...open, fields: new Map(open.fields).set(field, filled) });
}

/** One pane's hold on its tickets' drafts: `set` names the tickets it
 * holds open or on its Back trail, and a draft of a ticket no pane holds
 * goes. */
export function holdTicketDrafts(): { set(tickets: readonly string[]): void; release(): void } {
  const id = nextDraftHolder++;
  draftHolders.set(id, new Set());
  const prune = () => {
    for (const [k, { draft }] of drafts) {
      if (![...draftHolders.values()].some((held) => held.has(draft.ticket))) putDraft(k, null);
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

// A ticket's outcomes go with the last surface that draws them; a write
// still pending settles by the rule at the top.
onTrackerTicketClosed((ticket) => {
  const prefix = `${ticket}\n`;
  pruneUnshown((k) => k.startsWith(prefix));
});
onTrackerBoardDropped((board) => {
  pruneUnshown((_, w) => w.ref.backend === board.backend && w.ref.connection === board.connection);
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
  // The draft as it is sent: Save anyway is for it alone.
  consent.sending(k);
  writes.set(k, { ref: t.ref, target, pending: true, outcome: null, saveAnyway: null });
  const outcome = await sendWrite(t.ref.backend, computerName(t.ref.backend), TICKET_SUBJECT, () => send(acceptLoss));
  if (outcome.kind === 'applied') await Promise.all([trackerTicketSettled(t.key, comment), trackerBoardsSettled(t.ref.backend, t.ref.connection)]);
  // The ticket changed since it was read: read it again, so the person
  // sees what the tracker holds before trying again.
  if (outcome.kind === 'refused' && outcome.refusal.reason === 'stale') void refreshTrackerTicket(t.key);
  const loss = consent.landed(k, outcome, acceptLoss);
  if (outcome.kind === 'applied') drafts.delete(k);
  if (!targetShown(t.key, t.ref, target)) {
    writes.delete(k);
    toastAfterClose(t, outcome);
    return outcome;
  }
  const plain = outcome.kind === 'applied' && outcome.note === null;
  if (plain) writes.delete(k);
  else writes.set(k, { ref: t.ref, target, pending: false, outcome, saveAnyway: loss ? () => write(t, target, send, true, comment) : null });
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

/** Places the ticket on `board` between `prev` and `next`, the issues it
 * goes just after and just before, either empty for an end
 * (RankTrackerIssues). */
export function rankTicket(t: TicketWriteContext, board: string, prev: string, next: string): Promise<WriteOutcome | null> {
  return write(t, 'rank', () => RankTrackerIssues(t.ref.connection, { board, issues: [t.ref.issue], prev: prev || undefined, next: next || undefined }), false);
}

/** Moves the ticket into `sprint` of `board`, or to its backlog with
 * none (MoveTrackerIssuesToSprint). */
export function moveTicketToSprint(t: TicketWriteContext, board: string, sprint: string | null): Promise<WriteOutcome | null> {
  return write(t, 'sprint', () => MoveTrackerIssuesToSprint(t.ref.connection, { board, sprint: sprint ?? undefined, issues: [t.ref.issue] }), false);
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

/** The moves the ticket's workflow allows now, read from the tracker
 * (TrackerMoves): what a Board card's carry and Move to… offer. */
export function ticketMoves(t: TicketWriteContext): Promise<MoveView[]> {
  return withBackendTarget(t.ref.backend, () => TrackerMoves(t.ref.connection, t.ref.issue));
}

export function resetTicketWritesForTest(): void {
  writes.clear();
  drafts.clear();
  draftHolders.clear();
  consent.clear();
}
