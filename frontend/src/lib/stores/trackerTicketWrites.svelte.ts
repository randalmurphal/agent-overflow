// Every write the ticket screen, a Board card, a Backlog row and a
// dashboard's ticket list make, a board's own writes (its sprints, and the
// tickets made on it from the Backlog), and the reads their pickers and
// forms make
// (docs/architecture/tracker-backend.md, "Writes"). A card and a row write
// under the same ticket key as the ticket screen, so one write shows one
// state on all of them. A board's writes are held under the board's key
// (`boardWriteContext`) by the same runner and the same rules of where an
// outcome shows; they wait for the board's views to settle, and none of a
// ticket's rules (its view's settling, its read after a stale refusal,
// loss consent, drafts) is reachable from one, since those live in the
// ticket's `write` alone and it takes a ticket. Each write goes through
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
// (`holdTicketDrafts`, and a ticket pane's record): Close, another board,
// the trail's cap. A ticket moved into a pane of its own keeps its drafts:
// the pane's record holds it before the Issues pane lets it go.
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
// - a control draws its target's outcome while it is mounted
//   (`drawTicketWrite`: each `TicketWriteNote`, and the attachments row of
//   the file being sent), and draws it for the person only while its
//   surface is shown: the Board and a dashboard while on screen and no
//   ticket covers them, a ticket while its pane is on screen
//   (`components/issues/ticketWriteSurface.ts`);
// - a write that settles while a mounted control of its target is on a
//   shown surface keeps a refused, unknown or noted outcome there, on
//   every control of it, until the person dismisses it or no mounted
//   control of it remains; it is not said in a toast;
// - a write that settles while no control of its target is on a shown
//   surface (the Board under Time or under a ticket, a card scrolled out,
//   a closed ticket, a target the open ticket does not draw such as a
//   card's rank) is not kept, and a refused or unknown one is said in a
//   toast; so each outcome is shown once, under a control or in a toast;
// - a plain success shows nothing anywhere;
// - when the last mounted control of a target goes (its card scrolled out
//   or removed, the Board left for Time, its ticket closed), its settled
//   outcome goes, said nowhere; a pending write stays and settles by the
//   rules above. The check runs a microtask after the control goes, so a
//   control mounted anew in the same update (a waiting card back in its
//   own column) keeps it.
// Where it shows never changes what was written: a draft stays with its
// editor, and its loss consent is the same whether the refusal was kept
// or said in a toast.
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
//
// Files are attached one at a time per ticket, in the order given, each
// its own write under an `attach:` target so its outcome stays beside the
// next file's; the queue says which file is being sent and its place, and
// nothing else (no byte progress). A link added, a link removed and the
// poker writes are writes like any other; a vote, a reveal and a clear
// share one target. What a form reads (a connection's link types for the
// link form; its projects, a project's issue types and a type's create
// fields for New ticket) is held by the rules above `heldReads`.

import { untrack } from 'svelte';
import { SvelteMap } from 'svelte/reactivity';
import type {
  ChoiceListView,
  CreateRequest,
  DetailView,
  FieldEditInput,
  FieldView,
  IssueTypeView,
  LinkTypeView,
  LinkView,
  MoveView,
  ProjectView,
  SprintCompleteRequest,
  SprintEditRequest,
  SprintRequest,
  SprintStartRequest,
  UserListView,
  WriteView,
} from '../../../bindings/agent-overflow/internal/trackerapp/models';
import {
  AddTrackerComment,
  ClearTrackerPokerVotes,
  CompleteTrackerSprint,
  CreateTrackerIssue,
  CreateTrackerSprint,
  DeleteTrackerComment,
  EditTrackerComment,
  EditTrackerIssue,
  EditTrackerSprint,
  LinkTrackerIssues,
  MoveTrackerIssue,
  MoveTrackerIssuesToSprint,
  RankTrackerIssues,
  RevealTrackerPoker,
  SetTrackerEstimate,
  StartTrackerSprint,
  TrackerAssignableUsers,
  TrackerChoices,
  TrackerCreateFields,
  TrackerIssueTypes,
  TrackerProjectIssueTypes,
  TrackerLinkTypes,
  TrackerMoves,
  TrackerBoardProjects,
  UnlinkTrackerIssues,
  VoteTrackerPoker,
} from './bindings';
import { attachedBackendEntry, backendDisplayName } from './attachedBackends.svelte';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { onTicketPanesChanged, ticketPaneRefs } from './ticketPanes.svelte';
import { trackerBoardsSettled } from './trackerBoard.svelte';
import { addErrorToast, addToast } from './toast.svelte';
import {
  onTrackerTicketDetail,
  refreshTrackerTicket,
  trackerTicketDetail,
  trackerTicketKey,
  trackerTicketSettled,
  type CommentChange,
  type TrackerTicketRef,
} from './trackerTicket.svelte';
import { linkRequest, shownText, type LinkRelation } from './trackerTicketModel';
import { LossConsent, refusalSentence, sendWrite, TICKET_SUBJECT, type WriteOutcome, type WriteSubject } from './trackerWriteOutcome';
import { onBackendStatusChange } from './transportStatus.svelte';
import { TransferInterruptedError, TransferRefusedError, UploadNotSentError, uploadTrackerAttachmentBytes } from '../transport/attachmentTransfer';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { composeWorkspaceKey, workspaceKeyBackend } from '../utils/workspaceKey';
import { FORM_READ_AGE_MS } from '../pageTiming';

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

// Each ticket's writes by target, one box per ticket, so a write wakes the
// readers of its own ticket only (a Board card reads one box for every
// target it draws). A box is made by the ticket's first write and kept,
// empty once its writes settle, while the page lives.
const NO_WRITES: ReadonlyMap<string, TicketWrite> = new Map();
const writes = createKeyedSignalRegistry<ReadonlyMap<string, TicketWrite>>(NO_WRITES);

function putWrite(ticket: string, target: string, write: TicketWrite | null): void {
  const held = untrack(() => writes.get(ticket));
  if (!write && !held.has(target)) return;
  const next = new Map(held);
  if (write) next.set(target, write);
  else next.delete(target);
  writes.set(ticket, next.size === 0 ? NO_WRITES : next);
}

function heldWrite(ticket: string, target: string): TicketWrite | undefined {
  return untrack(() => writes.get(ticket)).get(target);
}

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
  /** Null until the ticket's detail is first held (a move picked on a
   * Board card opens the ticket with the form); then set once and kept
   * (`takeMoveBases`), whatever the ticket's detail becomes. */
  readonly bases: ReadonlyMap<string, string> | null;
  readonly fields: ReadonlyMap<string, MoveFieldDraft>;
}

type Draft = TicketDraft | TicketMoveDraft;

const drafts = new SvelteMap<string, { readonly draft: Draft }>();
// The tickets each pane holds open or on its Back trail, by holder.
const draftHolders = new Map<number, ReadonlySet<string>>();
let nextDraftHolder = 0;

// A ticket key never holds a NUL, so the first one ends it.
function writeKey(ticket: string, target: string): string {
  return `${ticket}\0${target}`;
}

function splitWriteKey(k: string): { ticket: string; target: string } {
  const at = k.indexOf('\0');
  return { ticket: k.slice(0, at), target: k.slice(at + 1) };
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

// The mounted controls of each write key, each with whether its surface
// is shown now.
const drawers = new Map<string, Set<() => boolean>>();

/** A mounted control of the ticket's target, on a surface shown while
 * `shown` answers true: the target's outcome is kept for it (the rule at
 * the top). Answers its release, for when the control unmounts. */
export function drawTicketWrite(ticket: string, target: string, shown: () => boolean): () => void {
  const k = writeKey(ticket, target);
  let set = drawers.get(k);
  if (!set) drawers.set(k, (set = new Set()));
  const mine = () => shown();
  set.add(mine);
  const held = set;
  return () => {
    held.delete(mine);
    if (held.size > 0) return;
    if (drawers.get(k) === held) drawers.delete(k);
    // A control mounted anew in the same update keeps the outcome.
    queueMicrotask(() => {
      if (!drawers.has(k) && heldWrite(ticket, target)?.pending === false) putWrite(ticket, target, null);
    });
  };
}

/** Whether a mounted control of the target is on a shown surface now. */
function targetShown(ticket: string, target: string): boolean {
  for (const shown of drawers.get(writeKey(ticket, target)) ?? []) {
    if (untrack(shown)) return true;
  }
  return false;
}

/** The target's last write; reactive. */
export function ticketWrite(ticket: string, target: string): TicketWrite | null {
  return writes.get(ticket).get(target) ?? null;
}

/** Every target's last write of the ticket; reactive, one read for all. */
export function ticketWrites(ticket: string): ReadonlyMap<string, TicketWrite> {
  return writes.get(ticket);
}

/** Drops a settled outcome the person read. */
export function dismissTicketWrite(ticket: string, target: string): void {
  if (heldWrite(ticket, target)?.pending === false) putWrite(ticket, target, null);
}

// A withdrawn offer drops its outcome; other outcomes stay, a lost answer
// among them.
const consent = new LossConsent((k) => {
  const { ticket, target } = splitWriteKey(k);
  if (heldWrite(ticket, target)?.saveAnyway) putWrite(ticket, target, null);
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

/** The comments of the ticket whose edit a person is writing; reactive. */
export function ticketCommentDrafts(ticket: string): ReadonlySet<string> {
  const prefix = writeKey(ticket, commentTarget(''));
  const ids = new Set<string>();
  for (const k of drafts.keys()) if (k.startsWith(prefix)) ids.add(k.slice(prefix.length));
  return ids;
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
 * starting from the text the ticket shows for it: now when the ticket's
 * detail is held, else when it first lands. */
export function openTicketMove(ticket: string, move: MoveView): void {
  putDraft(writeKey(ticket, 'status'), { kind: 'move', ticket, move, bases: null, fields: new Map() });
  const detail = trackerTicketDetail(ticket);
  if (detail) takeMoveBases(ticket, detail);
}

/** The one writer of a move form's edit bases: the text `detail` shows for
 * each long field of the move, taken once, by a form that has none yet. */
function takeMoveBases(ticket: string, detail: DetailView): void {
  const open = ticketMoveDraft(ticket);
  if (!open || open.bases) return;
  const long = (open.move.fields ?? []).filter((f) => f.kind === 'rich_text' && f.editable);
  putDraft(writeKey(ticket, 'status'), { ...open, bases: new Map(long.map((f) => [f.id, shownText(detail, f.id)])) });
}
onTrackerTicketDetail(takeMoveBases);

/** Fills one field of the open move form. */
export function setTicketMoveField(ticket: string, field: string, filled: MoveFieldDraft): void {
  const open = ticketMoveDraft(ticket);
  if (!open) return;
  putDraft(writeKey(ticket, 'status'), { ...open, fields: new Map(open.fields).set(field, filled) });
}

// A draft of a ticket no pane holds goes: no Issues pane's hold names it
// and no ticket pane shows it or has it on its Back trail.
function pruneDrafts(): void {
  const panes = new Set(ticketPaneRefs().map(trackerTicketKey));
  for (const [k, { draft }] of drafts) {
    if (panes.has(draft.ticket)) continue;
    if (![...draftHolders.values()].some((held) => held.has(draft.ticket))) putDraft(k, null);
  }
}
onTicketPanesChanged(pruneDrafts);

/** One Issues pane's hold on its tickets' drafts: `set` names the tickets
 * it holds open or on its Back trail. A ticket pane's tickets are held
 * through its record (`stores/ticketPanes.svelte#ticketPaneRefs`). */
export function holdTicketDrafts(): { set(tickets: readonly string[]): void; release(): void } {
  const id = nextDraftHolder++;
  draftHolders.set(id, new Set());
  return {
    set(tickets) {
      draftHolders.set(id, new Set(tickets));
      pruneDrafts();
    },
    release() {
      draftHolders.delete(id);
      pruneDrafts();
    },
  };
}

function computerName(backend: string): string {
  const entry = attachedBackendEntry(backend);
  return entry ? backendDisplayName(entry) : 'The ticket\'s computer';
}

interface WriteOptions {
  /** The comment an applied write changed (`trackerTicketSettled`). */
  readonly comment?: CommentChange;
  /** What a failure of `send` means when `send` is more than one call (an
   * upload's mint and transfer); null leaves it to the call's rule. */
  readonly failure?: (err: unknown) => WriteOutcome | null;
  /** What the write sends, as the person names it (a file's name). */
  readonly subject?: string;
  /** What it waits for once pending, before it is sent (the rank before
   * it in its column). */
  readonly after?: Promise<void>;
}

/** What a write is made to, as its writes are held: a ticket, or a
 * board's sprints and the tickets made on it (`BoardWriteContext`). */
interface WriteHolder {
  readonly key: string;
  readonly backend: BackendKey;
  readonly connection: string;
}

/** What its holder makes of a write's outcome: what it waits for before
 * "Saving" ends, and, once that settled, the second press it offers (Save
 * anyway) or null; the words of a toast. */
interface Landing {
  readonly words: WriteSubject;
  sending(): void;
  /** The views the outcome waits for, each asked now, in the order the
   * runner awaits them: the runner awaits each itself, so the outcome
   * changes in the same update as the view that lands last. */
  settle(outcome: WriteOutcome): readonly Promise<unknown>[];
  landed(outcome: WriteOutcome): (() => Promise<WriteOutcome | null>) | null;
  toast(outcome: WriteOutcome, subject: string | null): void;
}

/** The one runner of a held write: one pending write per holder and
 * target, an outcome kept under a shown control of the target or said in
 * a toast (the rules at the top). */
async function run(
  h: WriteHolder,
  target: string,
  send: () => Promise<WriteView>,
  landing: Landing,
  options: WriteOptions,
): Promise<WriteOutcome | null> {
  if (heldWrite(h.key, target)?.pending) return null;
  landing.sending();
  const subject = options.subject ?? null;
  putWrite(h.key, target, { pending: true, outcome: null, saveAnyway: null, subject });
  if (options.after) await options.after;
  const outcome = await sendWrite(h.backend, computerName(h.backend), landing.words, send, options.failure);
  for (const view of landing.settle(outcome)) await view;
  const again = landing.landed(outcome);
  if (!targetShown(h.key, target)) {
    putWrite(h.key, target, null);
    landing.toast(outcome, subject);
    return outcome;
  }
  const plain = outcome.kind === 'applied' && outcome.note === null;
  putWrite(h.key, target, plain ? null : { pending: false, outcome, saveAnyway: again, subject });
  return outcome;
}

/** A write to a ticket: its loss consent, its view's settling, a read of
 * it after a stale refusal, and its draft going once the write is made. */
function write(
  t: TicketWriteContext,
  target: string,
  send: (acceptLoss: boolean) => Promise<WriteView>,
  acceptLoss: boolean,
  options: WriteOptions = {},
): Promise<WriteOutcome | null> {
  const k = writeKey(t.key, target);
  return run({ key: t.key, backend: t.ref.backend, connection: t.ref.connection }, target, () => send(acceptLoss), {
    words: TICKET_SUBJECT,
    // The draft as it is sent: Save anyway is for it alone.
    sending: () => consent.sending(k),
    settle(outcome) {
      if (outcome.kind !== 'applied') return [];
      const boards = trackerBoardsSettled(t.ref.backend, t.ref.connection);
      const ticket = trackerTicketSettled(t.key, options.comment);
      // A comment's page read may fail while the boards are awaited: the
      // runner's await of it throws it.
      ticket.catch(() => undefined);
      return [boards, ticket];
    },
    landed(outcome) {
      // The ticket changed since it was read: read it again, so the person
      // sees what the tracker holds before trying again.
      if (outcome.kind === 'refused' && outcome.refusal.reason === 'stale') void refreshTrackerTicket(t.key);
      const loss = consent.landed(k, outcome, acceptLoss);
      if (outcome.kind === 'applied') drafts.delete(k);
      return loss ? () => write(t, target, send, true, options) : null;
    },
    toast: (outcome, subject) => toastUnshown(
      subject ? `${subject} for ${t.label}` : `the change to ${t.label}`,
      subject ? `Attaching ${subject} to ${t.label}` : `The change to ${t.label}`,
      outcome,
    ),
  }, options);
}

/** A write that settled with no control of its target on a shown
 * surface: said in a toast unless it was made. `change` names it inside
 * a sentence, `what` opens one. */
function toastUnshown(change: string, what: string, outcome: WriteOutcome): void {
  switch (outcome.kind) {
    case 'applied':
      if (outcome.unplaced) addToast('error', `${what}: ${outcome.note}`);
      return;
    case 'refused':
      addToast('error', `The tracker refused ${change}: ${refusalSentence(outcome.refusal, (id) => id)}`);
      return;
    case 'unknown':
    case 'not_sent':
    case 'lost':
      addToast('error', `${what}: ${outcome.message}`);
      return;
    case 'failed':
      addErrorToast(`${what} was not confirmed`, outcome.error);
  }
}

/** Sets fields of the ticket (EditTrackerIssue), `target` being the
 * control's; pending from the call, sent once `after` settles (a list's
 * batch waiting for its turn). */
export function editTicketFields(t: TicketWriteContext, target: string, fields: FieldEditInput[], after?: Promise<void>): Promise<WriteOutcome | null> {
  return write(t, target, (acceptLoss) => EditTrackerIssue(t.ref.connection, { issue: t.ref.issue, fields, accept_loss: acceptLoss }), false, { after });
}

/** Moves the ticket by one of its moves (MoveTrackerIssue); sent once
 * `after` settles, as editTicketFields. */
export function moveTicket(t: TicketWriteContext, move: string, fields?: FieldEditInput[], after?: Promise<void>): Promise<WriteOutcome | null> {
  return write(t, 'status', (acceptLoss) => MoveTrackerIssue(t.ref.connection, { issue: t.ref.issue, move, fields, accept_loss: acceptLoss }), false, { after });
}

/** Sets or clears the ticket's estimate on a board (SetTrackerEstimate). */
export function setTicketPoints(t: TicketWriteContext, board: string, value: number | null): Promise<WriteOutcome | null> {
  return write(t, 'points', () => SetTrackerEstimate(t.ref.connection, { board, issue: t.ref.issue, value: value ?? undefined }), false);
}

/** Why a rank found no place to send: said as not sent. */
export class RankPlaceError extends Error {}

/** Places the ticket on `board` between the issues `place` names when the
 * rank is sent (after `after`), the issues it goes just after and just
 * before, either empty for an end (RankTrackerIssues); `place` throws a
 * RankPlaceError when there is none. */
export function rankTicket(t: TicketWriteContext, board: string, place: () => { prev: string; next: string }, after?: Promise<void>): Promise<WriteOutcome | null> {
  return write(t, 'rank', () => {
    const { prev, next } = place();
    return RankTrackerIssues(t.ref.connection, { board, issues: [t.ref.issue], prev: prev || undefined, next: next || undefined });
  }, false, { after, failure: placeFailure });
}

/** Where a placed write goes, named when it is sent (after `after`): the
 * issues it goes just after and just before, either empty for an end;
 * `place` throws a RankPlaceError when there is none. */
export interface TicketPlace {
  readonly place: () => { prev: string; next: string };
  readonly after?: Promise<void>;
}

function placeFailure(err: unknown): WriteOutcome | null {
  return err instanceof RankPlaceError ? { kind: 'not_sent', message: err.message } : null;
}

/** Moves the ticket into `sprint` of `board`, or to its backlog with
 * none (MoveTrackerIssuesToSprint), at `at` when given. */
export function moveTicketToSprint(t: TicketWriteContext, board: string, sprint: string | null, at?: TicketPlace): Promise<WriteOutcome | null> {
  return write(t, 'sprint', () => {
    const { prev, next } = at ? at.place() : { prev: '', next: '' };
    return MoveTrackerIssuesToSprint(t.ref.connection, { board, sprint: sprint ?? undefined, issues: [t.ref.issue], prev: prev || undefined, next: next || undefined });
  }, false, { after: at?.after, failure: placeFailure });
}

/** A board as what its sprint writes and the tickets made on it are made
 * to: their writes are held under its key, apart from every ticket's, and
 * none of a ticket's rules (its view's settling, its read after a stale
 * refusal, loss consent) is reached from one. */
export interface BoardWriteContext {
  readonly key: string;
  readonly backend: BackendKey;
  readonly connection: string;
  /** The tracker board. */
  readonly board: string;
}

export function boardWriteContext(ref: { readonly backend: BackendKey; readonly connection: string; readonly board: string }): BoardWriteContext {
  // Three parts, so no ticket key (connection and issue) is one.
  return { key: composeWorkspaceKey(ref.backend, `${ref.connection}\n${ref.board}\nboard`), backend: ref.backend, connection: ref.connection, board: ref.board };
}

const BOARD_SUBJECT: WriteSubject = {
  look: 'the board',
  shows: 'The board',
  ended: 'this board no longer follows it',
  reads: () => true,
};

/** The target of a sprint's writes (edit, start, complete), one at a time
 * per sprint; `new` is the sprint being created. */
export function sprintTarget(sprint: string): string {
  return `sprint:${sprint}`;
}

/** The target of a ticket made in a list (`trackerBoardModel.ts#sectionList`). */
export function createTarget(list: string): string {
  return `create:${list}`;
}

/** A write to a board: once made it waits for the board's views to hold
 * what their computer was told, and it is said in a toast as `what` (a
 * sentence's opening: "Starting Sprint 3") when no control of it shows. */
function boardWrite(b: BoardWriteContext, target: string, what: string, send: () => Promise<WriteView>): Promise<WriteOutcome | null> {
  return run(b, target, send, {
    words: BOARD_SUBJECT,
    sending: () => {},
    settle: (outcome) => (outcome.kind === 'applied' ? [trackerBoardsSettled(b.backend, b.connection)] : []),
    landed: () => null,
    toast: (outcome) => toastUnshown(what.charAt(0).toLowerCase() + what.slice(1), what, outcome),
  }, {});
}

/** Makes a ticket on the board in the section `list` (its sprint, or the
 * backlog with none). `where` names the section in a toast. */
export function createTicket(b: BoardWriteContext, list: string, where: string, req: Omit<CreateRequest, 'board'>): Promise<WriteOutcome | null> {
  return boardWrite(b, createTarget(list), `Creating a ticket in ${where}`, () => CreateTrackerIssue(b.connection, { ...req, board: b.board }));
}

/** Makes a future sprint on the board. */
export function createSprint(b: BoardWriteContext, req: Omit<SprintRequest, 'board'>): Promise<WriteOutcome | null> {
  return boardWrite(b, sprintTarget('new'), `Creating ${req.name}`, () => CreateTrackerSprint(b.connection, { ...req, board: b.board }));
}

/** Changes the members of a sprint that `req` sets. */
export function editSprint(b: BoardWriteContext, name: string, req: Omit<SprintEditRequest, 'board'>): Promise<WriteOutcome | null> {
  return boardWrite(b, sprintTarget(req.sprint), `Changing ${name}`, () => EditTrackerSprint(b.connection, { ...req, board: b.board }));
}

/** Starts a future sprint, with its dates and, when set, its name and goal. */
export function startSprint(b: BoardWriteContext, name: string, req: Omit<SprintStartRequest, 'board'>): Promise<WriteOutcome | null> {
  return boardWrite(b, sprintTarget(req.sprint), `Starting ${name}`, () => StartTrackerSprint(b.connection, { ...req, board: b.board }));
}

/** Completes an active sprint, its unfinished tickets moved to `move_to`
 * (an open sprint) or to the backlog with none. */
export function completeSprint(b: BoardWriteContext, name: string, req: Omit<SprintCompleteRequest, 'board'>): Promise<WriteOutcome | null> {
  return boardWrite(b, sprintTarget(req.sprint), `Completing ${name}`, () => CompleteTrackerSprint(b.connection, { ...req, board: b.board }));
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
  /** Its write's target, which the row saying it is being sent draws. */
  readonly target: string;
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
  const out: { target: string; name: string; outcome: WriteOutcome }[] = [];
  for (const [target, w] of writes.get(ticket)) {
    if (!target.startsWith(ATTACH_TARGET_PREFIX) || !w.outcome) continue;
    out.push({ target, name: w.subject ?? '', outcome: w.outcome });
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
      const target = `${ATTACH_TARGET_PREFIX}${nextUpload++}`;
      uploads.set(t.key, { name: file.name, size: file.size, index: queue.sent, total: queue.sent + queue.files.length, target });
      const sending = file;
      await write(t, target, () => uploadTrackerAttachmentBytes(t.ref.backend, t.ref.connection, t.ref.issue, sending), false, { failure: uploadFailure, subject: file.name });
    }
  } finally {
    uploadQueues.delete(t.key);
    uploads.delete(t.key);
  }
}

/** What a failed upload means, from where it failed (attachmentTransfer.ts)
 * and the route's code:
 * - Before the PUT left (the mint, the route to the computer): not sent,
 *   a failed call whose error gives the reason and keeps the mint's code.
 * - Answered with a code that says the write may have been made
 *   (`UNCLEAR_UPLOAD_CODES`): unclear, in the route's sentence.
 * - Answered with another refusal the route worded: not attached, in its
 *   sentence.
 * - Broken off, or a failure the route did not word: unclear.
 * A refusal by the tracker itself is part of the write's answer
 * (`writeOutcome`). */
function uploadFailure(err: unknown): WriteOutcome | null {
  if (err instanceof UploadNotSentError) return { kind: 'failed', error: err };
  if (err instanceof TransferRefusedError && err.code !== null && UNCLEAR_UPLOAD_CODES.has(err.code)) return { kind: 'unknown', message: err.message };
  if (err instanceof TransferRefusedError && err.status < 500) return { kind: 'not_sent', message: err.message };
  if (err instanceof TransferRefusedError || err instanceof TransferInterruptedError) return { kind: 'unknown', message: UPLOAD_UNKNOWN };
  return null;
}

/** The route's codes for an attach that may have been made: a tracker
 * answer the computer could not read, and the attach's budget running out
 * (internal/app/app_tracker_writes.go `trackerWriteError`). */
const UNCLEAR_UPLOAD_CODES: ReadonlySet<string> = new Set(['tracker_write_unclear', 'method_budget_exceeded']);

const UPLOAD_UNKNOWN = 'The file may or may not be attached; look at the ticket\'s attachments before attaching it again.';

/** What a form read of a connection answered last; reactive. */
export interface HeldRead<T> {
  readonly value: T | null;
  readonly loading: boolean;
  readonly error: unknown;
}

// A form read (a connection's link types for the link form; a board's
// projects, a project's issue types and a type's create fields for a
// ticket made on the Backlog) must keep true:
// - Only a person's act reads it: its form opening or coming back into
//   view, a pick that needs it, its Retry, its screen's Refresh
//   (`refreshFormReads`), or Add refused for fields the screen now
//   requires (`reloadCreateFields`). Nothing reads it again because a
//   read settled, so a caller starts the read outside any reactive scope
//   that reads it.
// - What was read is held, and an act reads it again once it is older
//   than FORM_READ_AGE_MS, or after Refresh or a reconnect. While that
//   read is out the held value shows; its answer replaces it, and a
//   failure is held with its error beside the value, shown by the form,
//   until the next act reads it again.
// - One read per key is out at a time, and a second ask while it is out
//   waits for its answer. Only the read that set an entry settles it, so
//   a read from before a reconnect settles nothing.
const heldReads = new SvelteMap<string, HeldEntry<unknown>>();
// Each connection's Refreshes counted, which a form on screen follows.
const formRefreshes = new SvelteMap<string, number>();

/** A held read as the map holds it: the connection it was read on
 * (`composeWorkspaceKey`), when its value was read (0 once Refresh made
 * it old), and, while being read, the read's settling, so a second ask
 * waits for the same answer. */
interface HeldEntry<T> extends HeldRead<T> {
  readonly connection: string;
  readonly at: number;
  readonly settled?: Promise<void>;
}

function heldRead<T>(key: string): HeldRead<T> | null {
  return (heldReads.get(key) as HeldRead<T> | undefined) ?? null;
}

/** Reads `key` of `connection` on `backend` unless it is being read or
 * was read within FORM_READ_AGE_MS. The caller is a person's act (see
 * above). Settles once the key holds the answer of the read out, whoever
 * asked. */
function loadHeldRead<T>(backend: BackendKey, connection: string, key: string, read: () => Promise<T>): Promise<void> {
  const held = untrack(() => heldReads.get(key)) as HeldEntry<T> | undefined;
  if (held?.settled) return held.settled;
  if (held?.value != null && Date.now() - held.at < FORM_READ_AGE_MS) return Promise.resolve();
  let settle!: () => void;
  const settled = new Promise<void>((resolve) => { settle = resolve; });
  const of = composeWorkspaceKey(backend, connection);
  const shown = held?.value ?? null;
  const at = held?.at ?? 0;
  const reading: HeldEntry<T> = { value: shown, loading: true, error: null, connection: of, at, settled };
  heldReads.set(key, reading);
  void (async () => {
    try {
      const value = await withBackendTarget(backend, read);
      if (heldReads.get(key) === reading) heldReads.set(key, { value, loading: false, error: null, connection: of, at: Date.now() });
    } catch (err) {
      if (heldReads.get(key) === reading) heldReads.set(key, { value: shown, loading: false, error: err, connection: of, at });
    } finally {
      settle();
    }
  })();
  return settled;
}

/** Makes the entry at `key` old, unless it is being read: the next act
 * reads it again. */
function makeOld(key: string): void {
  const held = heldReads.get(key);
  if (held && !held.settled) heldReads.set(key, { ...held, at: 0 });
}

/** The screen's Refresh: what the connection's forms hold is read again
 * by the next act, and at once by a form on screen (`readOnFormRefresh`). */
export function refreshFormReads(backend: BackendKey, connection: string): void {
  const of = composeWorkspaceKey(backend, connection);
  for (const [key, held] of heldReads) if (held.connection === of) makeOld(key);
  formRefreshes.set(of, (formRefreshes.get(of) ?? 0) + 1);
}

/** Runs a form's `read` after each Refresh of its connection's form
 * reads while the form is on screen; its mount runs nothing here. Called
 * from the form's setup. */
export function readOnFormRefresh(of: () => { readonly backend: BackendKey; readonly connection: string }, read: () => void): void {
  let seen: { readonly key: string; readonly n: number } | null = null;
  $effect(() => {
    const { backend, connection } = of();
    const key = composeWorkspaceKey(backend, connection);
    const n = formRefreshes.get(key) ?? 0;
    if (seen?.key === key && seen.n !== n) untrack(read);
    seen = { key, n };
  });
}

// A computer that connects again may answer otherwise.
onBackendStatusChange((backend, status) => {
  if (status.status !== 'connected') return;
  for (const key of heldReads.keys()) if (workspaceKeyBackend(key) === backend) heldReads.delete(key);
});

function readKey(backend: BackendKey, ...parts: string[]): string {
  return composeWorkspaceKey(backend, parts.join('\n'));
}

/** The connection's link types; null before `loadTicketLinkTypes`. */
export function ticketLinkTypes(backend: BackendKey, connection: string): HeldRead<LinkTypeView[]> | null {
  return heldRead(readKey(backend, 'links', connection));
}

export function loadTicketLinkTypes(backend: BackendKey, connection: string): Promise<void> {
  return loadHeldRead(backend, connection, readKey(backend, 'links', connection), () => TrackerLinkTypes(connection));
}

/** The projects the board shows, which a ticket made on it is made in;
 * null before `loadBoardProjects`. */
export function boardProjects(b: BoardWriteContext): HeldRead<ProjectView[]> | null {
  return heldRead(readKey(b.backend, 'projects', b.connection, b.board));
}

export function loadBoardProjects(b: BoardWriteContext): Promise<void> {
  return loadHeldRead(b.backend, b.connection, readKey(b.backend, 'projects', b.connection, b.board), () => TrackerBoardProjects(b.connection, b.board));
}

/** The types a ticket can be made with in `project`; null before
 * `loadIssueTypes`. */
export function issueTypes(backend: BackendKey, connection: string, project: string): HeldRead<IssueTypeView[]> | null {
  return heldRead(readKey(backend, 'types', connection, project));
}

export function loadIssueTypes(backend: BackendKey, connection: string, project: string): Promise<void> {
  return loadHeldRead(backend, connection, readKey(backend, 'types', connection, project), () => TrackerIssueTypes(connection, project));
}

/** The types `project` has, which a person who can see it reads whatever
 * they may create (the Credit group's support types); null before
 * `loadProjectIssueTypes`. */
export function projectIssueTypes(backend: BackendKey, connection: string, project: string): HeldRead<IssueTypeView[]> | null {
  return heldRead(readKey(backend, 'project types', connection, project));
}

export function loadProjectIssueTypes(backend: BackendKey, connection: string, project: string): Promise<void> {
  return loadHeldRead(backend, connection, readKey(backend, 'project types', connection, project), () => TrackerProjectIssueTypes(connection, project));
}

/** The fields a ticket of `type` made in `project` takes besides its
 * title, the required ones marked; null before `loadCreateFields`. */
export function createFields(backend: BackendKey, connection: string, project: string, type: string): HeldRead<FieldView[]> | null {
  return heldRead(readKey(backend, 'fields', connection, project, type));
}

export function loadCreateFields(backend: BackendKey, connection: string, project: string, type: string): Promise<void> {
  return loadHeldRead(backend, connection, readKey(backend, 'fields', connection, project, type), () => TrackerCreateFields(connection, project, type));
}

/** Reads the create fields again: the person's act after the tracker
 * refused a create for fields it requires, since its screen changed. */
export function reloadCreateFields(backend: BackendKey, connection: string, project: string, type: string): Promise<void> {
  makeOld(readKey(backend, 'fields', connection, project, type));
  return loadCreateFields(backend, connection, project, type);
}

/** The values `field` allows on a ticket of `type` made in `project` whose
 * name starts with `text`. */
export function createChoices(b: BoardWriteContext, project: string, type: string, field: string, text: string): Promise<ChoiceListView> {
  return withBackendTarget(b.backend, () => TrackerChoices(b.connection, { project, type }, field, text));
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
  writes.reset();
  drawers.clear();
  drafts.clear();
  draftHolders.clear();
  uploadQueues.clear();
  uploads.clear();
  heldReads.clear();
  formRefreshes.clear();
  consent.clear();
}
