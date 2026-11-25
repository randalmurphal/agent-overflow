// The Issues pane's view state (docs/specs/issues.md, "The Issues pane"):
// the board a pane shows and the ticket open over or beside it
// (`createBoardSelection`), which card fields each board shows (Display),
// Only mine, the column a compact screen shows, and the ticket's
// remembered choices: over or beside, which sections are open, and the
// comment order. The board a person picked last and Only mine are saved
// for this client (frontendStorage) and seed the next pane that opens;
// Display follows another window of the client as it changes; the compact
// column lasts while the app runs. No computer is asked for any of it. The
// subscriptions themselves are trackerBoard.svelte.ts's and
// trackerTicket.svelte.ts's, the boards to pick from
// trackerBoardChoices.svelte.ts's.

import { untrack } from 'svelte';
import { SvelteMap } from 'svelte/reactivity';
import { getAttachedBackends } from './attachedBackends.svelte';
import { onFrontendValueChanged, readFrontendValue, writeFrontendValue } from './frontendStorage';
import {
  attachTrackerBoard,
  renewEndedTrackerBoard,
  setTrackerBoardShown,
  trackerBoardOnComputer,
  type TrackerBoardAttachment,
  type TrackerBoardRef,
} from './trackerBoard.svelte';
import { attachTrackerTicket, trackerTicketKey, type TrackerTicketAttachment, type TrackerTicketRef } from './trackerTicket.svelte';
import { holdTicketDrafts } from './trackerTicketWrites.svelte';
import type { CommentOrder } from './trackerTicketModel';
import {
  acquireTrackerBoardChoices,
  isBoardChoiceOpenable,
  trackerBoardChoiceComputers,
  trackerBoardChoicesAnsweredBy,
  trackerBoardChoicesOf,
  trackerBoardChoicesReadStarted,
  type ComputerChoices,
  type ListedBoardChoice,
} from './trackerBoardChoices.svelte';
import { pageGrantsKnownTracked, scopeGrant } from '../transport/scopes';
import type { CapturedError } from './errorReports.svelte';
import type { BoardChoice, CardView } from '../../../bindings/agent-overflow/internal/trackerapp/models';

/** A card field the Display menu turns on and off. Title always shows. */
export type CardField = 'type' | 'key' | 'parent' | 'labels' | 'priority' | 'estimate' | 'assignee' | 'days';

export const CARD_FIELDS: readonly { readonly field: CardField; readonly label: string }[] = [
  { field: 'type', label: 'Type' },
  { field: 'key', label: 'Key' },
  { field: 'parent', label: 'Epic or parent' },
  { field: 'labels', label: 'Labels' },
  { field: 'priority', label: 'Priority' },
  { field: 'estimate', label: 'Estimate' },
  { field: 'assignee', label: 'Assignee' },
  { field: 'days', label: 'Days in status' },
];

const FIELD_NAMES: ReadonlySet<string> = new Set(CARD_FIELDS.map((f) => f.field));

const BOARD_KEY = 'issues-board';
const HIDDEN_FIELDS_KEY = 'issues-hidden-fields';
const ONLY_MINE_KEY = 'issues-only-mine';
const PLACEMENT_KEY = 'issues-ticket-placement';
const SECTIONS_KEY = 'issues-ticket-folds';
const COMMENT_ORDER_KEY = 'issues-ticket-comment-order';
// Boards whose Display was changed, newest last; the oldest goes past this.
const MAX_BOARDS_REMEMBERED = 200;
// Sections whose fold was changed, newest last; the oldest goes past this.
const MAX_SECTIONS_REMEMBERED = 200;
// Tickets a pane steps back through, each opened from the one before.
const MAX_TICKET_TRAIL = 20;

function isString(v: unknown): v is string {
  return typeof v === 'string';
}

function readBoard(): TrackerBoardRef | null {
  const v = readFrontendValue(BOARD_KEY) as Partial<TrackerBoardRef> | null;
  if (!v || typeof v !== 'object') return null;
  const { backend, projectId, connection, board } = v;
  if (!isString(backend) || !isString(projectId) || !isString(connection) || !isString(board)) return null;
  if (!projectId || !connection || !board) return null;
  return { backend, projectId, connection, board };
}

/** Each board's hidden fields, by trackerBoardOnComputer, oldest first. */
function readHiddenFields(): ReadonlyMap<string, ReadonlySet<CardField>> {
  const v = readFrontendValue(HIDDEN_FIELDS_KEY);
  if (!Array.isArray(v)) return new Map();
  const out = new Map<string, ReadonlySet<CardField>>();
  for (const entry of v.slice(-MAX_BOARDS_REMEMBERED)) {
    if (!Array.isArray(entry) || entry.length !== 2 || !isString(entry[0]) || !Array.isArray(entry[1])) continue;
    out.set(entry[0], new Set(entry[1].filter((f): f is CardField => isString(f) && FIELD_NAMES.has(f))));
  }
  return out;
}

let hiddenFields = $state.raw(readHiddenFields());
const compactColumns = new SvelteMap<string, number>();

onFrontendValueChanged(HIDDEN_FIELDS_KEY, () => { hiddenFields = readHiddenFields(); });

/** The board this client's person picked last; null before one was. Read
 * when a pane opens. */
export function pickedBoard(): TrackerBoardRef | null {
  return readBoard();
}

export function sameBoard(a: TrackerBoardRef | null, b: TrackerBoardRef | null): boolean {
  return a === b || (!!a && !!b && a.backend === b.backend && a.projectId === b.projectId && a.connection === b.connection && a.board === b.board);
}

function savePick(ref: TrackerBoardRef): void {
  writeFrontendValue(BOARD_KEY, { backend: ref.backend, projectId: ref.projectId, connection: ref.connection, board: ref.board });
}

const NOTHING_HIDDEN: ReadonlySet<CardField> = new Set();

/** The fields hidden on a board's cards; reactive. */
export function hiddenCardFields(ref: TrackerBoardRef): ReadonlySet<CardField> {
  return hiddenFields.get(trackerBoardOnComputer(ref)) ?? NOTHING_HIDDEN;
}

export function setCardFieldShown(ref: TrackerBoardRef, field: CardField, shown: boolean): void {
  const key = trackerBoardOnComputer(ref);
  const hidden = new Set(hiddenFields.get(key));
  if (shown) hidden.delete(field);
  else hidden.add(field);
  const next = new Map(hiddenFields);
  // Re-inserted so the board changed last is kept longest.
  next.delete(key);
  if (hidden.size > 0) next.set(key, hidden);
  while (next.size > MAX_BOARDS_REMEMBERED) next.delete(next.keys().next().value!);
  if (writeFrontendValue(HIDDEN_FIELDS_KEY, [...next].map(([k, fields]) => [k, [...fields]]))) hiddenFields = next;
}

/** Only mine as this client last set it: what a pane opens with. */
export function savedOnlyMine(): boolean {
  return readFrontendValue(ONLY_MINE_KEY) === true;
}

export function saveOnlyMine(on: boolean): void {
  writeFrontendValue(ONLY_MINE_KEY, on);
}

/** The column a compact screen shows of a board, null before one was
 * picked this session. */
export function compactColumn(ref: TrackerBoardRef): number | null {
  return compactColumns.get(trackerBoardOnComputer(ref)) ?? null;
}

export function setCompactColumn(ref: TrackerBoardRef, column: number): void {
  compactColumns.set(trackerBoardOnComputer(ref), column);
}

/** Where a screen shows a ticket: over the list, or beside it. */
export type TicketPlacement = 'over' | 'beside';

/** The screens that open a ticket; only the Board is built. */
type TicketScreen = 'board';

function readPlacements(): Partial<Record<TicketScreen, TicketPlacement>> {
  const v = readFrontendValue(PLACEMENT_KEY) as Record<string, unknown> | null;
  return v && typeof v === 'object' && (v.board === 'over' || v.board === 'beside') ? { board: v.board } : {};
}

let placements = $state.raw(readPlacements());

/** Where the screen shows a ticket when the pane is wide enough for
 * beside; over until the person chose. Reactive. */
export function ticketPlacement(screen: TicketScreen): TicketPlacement {
  return placements[screen] ?? 'over';
}

export function setTicketPlacement(screen: TicketScreen, placement: TicketPlacement): void {
  const next = { ...placements, [screen]: placement };
  if (writeFrontendValue(PLACEMENT_KEY, next)) placements = next;
}

/** A section of a ticket on its computer: a tracker field's ID (a Jira
 * field is the site's, so its fold holds on every ticket of the
 * connection) or one of the screen's own (`more-sections`, `more-fields`,
 * `history`). */
export interface TicketSection {
  readonly backend: string;
  readonly connection: string;
  readonly section: string;
}

function sectionKeyOf(s: TicketSection): string {
  return `${s.backend}\n${s.connection}\n${s.section}`;
}

/** Each section's fold as the person set it, by sectionKeyOf, oldest first. */
function readSections(): ReadonlyMap<string, boolean> {
  const v = readFrontendValue(SECTIONS_KEY);
  if (!Array.isArray(v)) return new Map();
  const out = new Map<string, boolean>();
  for (const entry of v.slice(-MAX_SECTIONS_REMEMBERED)) {
    if (Array.isArray(entry) && entry.length === 2 && isString(entry[0]) && typeof entry[1] === 'boolean') out.set(entry[0], entry[1]);
  }
  return out;
}

let sections = $state.raw(readSections());

/** Whether a section is open: as the person left it, else `opens`.
 * Reactive. */
export function ticketSectionOpen(s: TicketSection, opens: boolean): boolean {
  return sections.get(sectionKeyOf(s)) ?? opens;
}

export function setTicketSectionOpen(s: TicketSection, open: boolean): void {
  const key = sectionKeyOf(s);
  const next = new Map(sections);
  // Re-inserted so the section changed last is kept longest.
  next.delete(key);
  next.set(key, open);
  while (next.size > MAX_SECTIONS_REMEMBERED) next.delete(next.keys().next().value!);
  if (writeFrontendValue(SECTIONS_KEY, [...next])) sections = next;
}

let commentOrder = $state<CommentOrder>(readFrontendValue(COMMENT_ORDER_KEY) === 'newest' ? 'newest' : 'oldest');

/** The order comments read in, oldest first until the person chose.
 * Reactive. */
export function ticketCommentOrder(): CommentOrder {
  return commentOrder;
}

export function setTicketCommentOrder(order: CommentOrder): void {
  if (writeFrontendValue(COMMENT_ORDER_KEY, order)) commentOrder = order;
}

/** What the pane knew of a ticket when it opened, drawn until its view is
 * read: the card clicked on the board, or the parent a ticket named. */
export interface TicketSeed {
  readonly key: string;
  readonly title: string;
  readonly card: CardView | null;
}

/** A ticket open in a pane. */
export interface OpenTicket {
  readonly ref: TrackerTicketRef;
  readonly seed: TicketSeed;
}

/** A ticket a surface outside the pane asked it to open (a thread's
 * link): its ticket, what to draw until it is read, and the project the
 * ask came from, whose boards are tried first. `unplaced` runs when no
 * board of the ticket's connection can be opened: with the failure when
 * the computer's boards could not be read, else null (none lists one). */
export interface TicketOpenRequest {
  readonly ref: TrackerTicketRef;
  readonly seed: TicketSeed;
  readonly project: string | null;
  readonly unplaced: (failure: CapturedError | null) => void;
}

// The one ticket asked for and not yet taken by a pane; a later ask
// replaces it.
let ticketOpenRequest = $state.raw<TicketOpenRequest | null>(null);

/** Asks the Issues pane to open a ticket, which the pane takes when it
 * mounts or at once when it is open. */
export function requestTicketOpen(request: TicketOpenRequest): void {
  ticketOpenRequest = request;
}

/** The ticket asked for, taken: the next call answers null. Reactive
 * until taken. */
export function takeTicketOpenRequest(): TicketOpenRequest | null {
  const request = ticketOpenRequest;
  if (request) ticketOpenRequest = null;
  return request;
}

/** Whether a ticket is asked for and not yet taken; reactive. */
export function ticketOpenRequested(): boolean {
  return ticketOpenRequest !== null;
}

/**
 * The board a ticket of `connection` on `backend` opens over, from that
 * computer's boards: the first openable board of `project` listing it,
 * else of a project that is not archived, else of any; null when no
 * openable board of the connection is listed.
 */
export function boardForTicket(
  rows: readonly BoardChoice[],
  backend: string,
  connection: string,
  project: string | null,
): TrackerBoardRef | null {
  const open = rows.filter((c) => c.connection === connection && isBoardChoiceOpenable(c));
  const ofProject = open.filter((c) => c.project === project);
  const pick = ofProject.find((c) => c.opens_first) ?? ofProject[0] ?? open.find((c) => !c.project_archived) ?? open[0];
  return pick ? choiceRef({ backend, choice: pick }) : null;
}

/** What the opening rule decided: a board, or none, and whether it still
 * waits on a computer's boards. */
export interface OpeningDecision {
  readonly board: TrackerBoardRef | null;
  readonly waiting: boolean;
}

/**
 * The board a pane opens on when the person has picked none, from what the
 * computers answered (`computers`, this computer first, then the others in
 * the order the app lists them): the board that opens first of the project
 * the pane was opened from, else that project's first openable one, as soon
 * as its computer answered; else, once every computer has answered or
 * could not be read, the same of the first project that is not archived
 * and lists an openable board, in the computers' order and each computer's
 * own (by project name, then project ID, then the project's order of its
 * boards), or of the first archived one when no other lists one; else
 * none. A board that cannot be opened (`isBoardChoiceOpenable`) is never
 * chosen.
 */
export function openingBoard(
  computers: readonly { readonly backend: string; readonly state: ComputerChoices }[],
  openedFromProject: string | null,
): OpeningDecision {
  const choices: ListedBoardChoice[] = computers.flatMap(({ backend, state }) => (state.rows ?? []).map((choice) => ({ backend, choice })));
  const firstOf = (backend: string, project: string): TrackerBoardRef | null => {
    const open = choices.filter((c) => c.backend === backend && c.choice.project === project && isBoardChoiceOpenable(c.choice));
    const pick = open.find((c) => c.choice.opens_first) ?? open[0];
    return pick ? choiceRef(pick) : null;
  };
  if (openedFromProject) {
    // A project's boards come from its one computer: rows mean it answered.
    const from = choices.find((c) => c.choice.project === openedFromProject);
    const board = from ? firstOf(from.backend, openedFromProject) : null;
    if (board) return { board, waiting: false };
  }
  if (computers.some((c) => c.state.kind === 'reading')) return { board: null, waiting: true };
  const openable = choices.filter((c) => isBoardChoiceOpenable(c.choice));
  const first = openable.find((c) => !c.choice.project_archived) ?? openable[0];
  return { board: first ? firstOf(first.backend, first.choice.project) : null, waiting: false };
}

/** The board a pane shows and the ticket open on it, and the
 * subscriptions that show them. */
export interface BoardSelection {
  /** The board shown; null while the opening rule waits or when it found
   * none. Reactive. */
  readonly board: TrackerBoardRef | null;
  /** Its subscription, null with no board. Reactive. */
  readonly attachment: TrackerBoardAttachment | null;
  /** The opening rule is waiting on a computer's boards. Reactive. */
  readonly choosing: boolean;
  /** The ticket open over or beside the board; null for none. Reactive. */
  readonly ticket: OpenTicket | null;
  /** Its subscription, null with no ticket. Reactive. */
  readonly ticketAttachment: TrackerTicketAttachment | null;
  /** Where the ticket shows now: beside only when the pane fits it and the
   * person chose it. Reactive. */
  readonly placement: TicketPlacement;
  /** Whether the person sees the board: the pane is on screen and no
   * ticket covers it. The board's subscription votes this. Reactive. */
  readonly boardShown: boolean;
  /** How many tickets Back steps through before it closes the ticket.
   * Reactive. */
  readonly trail: number;
  /** The person picked `ref`: shown and saved for the next pane. A pick of
   * the board shown while it has ended subscribes it again. */
  pick(ref: TrackerBoardRef): void;
  /** Opens issue `issue` of the board's connection: from the board, or
   * from the ticket open now, which Back then returns to. */
  openTicket(issue: string, seed: TicketSeed, from: 'board' | 'ticket'): void;
  /** Opens a ticket asked for from outside the pane: over the board
   * shown when it is of the ticket's computer and connection, else over
   * the board `boardForTicket` picks once that computer's boards are
   * read, which it shows without saving it as the person's pick. No such
   * board runs the request's `unplaced`. */
  openRequested(request: TicketOpenRequest): void;
  /** Back: the ticket this one was opened from, else closes it. */
  stepBack(): void;
  /** Close: the ticket goes, and the tickets it was opened from. */
  closeTicket(): void;
  destroy(): void;
}

export interface BoardSelectionOptions {
  /** The project of the thread the pane was opened from. */
  readonly openedFromProject: string | null;
  /** Whether the pane is on screen now; reactive. */
  readonly shown: () => boolean;
  /** Whether the pane fits a ticket beside the board now; reactive. */
  readonly besideFits: () => boolean;
}

/** Where a board's computer stands for the pane: `waiting` until the page
 * knows its grants, `gone` when it is not attached or refuses tracker:read
 * (it can never answer), else `ready`. Reactive. */
function boardComputer(backend: string): 'waiting' | 'gone' | 'ready' {
  if (!pageGrantsKnownTracked()) return 'waiting';
  if (!getAttachedBackends().some((entry) => entry.id === backend)) return 'gone';
  const grant = scopeGrant('tracker:read', backend);
  return grant === 'granted' ? 'ready' : grant === 'refused' ? 'gone' : 'waiting';
}

/**
 * One pane's board, chosen once: the person's pick, else the board picked
 * last (opened at once, beside the read of the boards), else the opening
 * rule. Nothing else changes it: another window, a later answer, a board
 * that ends. Its computer is the authority on whether it opens: when it
 * refuses or ends the board picked last before the pane showed any of its
 * cards, the selection lets it go and the opening rule chooses; so does a
 * computer that is forgotten or refuses tracker:read, at any time and with
 * no call. An ended board is subscribed again once an answer of its
 * computer read after the end lists it as openable (a connection turned
 * back on). A board its project stops listing while another project of the
 * same computer lists it moves to that project's listing, which subscribes
 * again under it. Holds the board choices and the subscription until
 * `destroy`; create it in a component's setup. The states and what moves
 * them are in docs/specs/issues.md, "Pane".
 */
export function createBoardSelection(opts: BoardSelectionOptions): BoardSelection {
  const releaseChoices = acquireTrackerBoardChoices();
  // The drafts of the ticket open and those on the Back trail live on.
  const drafts = holdTicketDrafts();
  let board = $state.raw<TrackerBoardRef | null>(pickedBoard());
  let attachment = $state.raw<TrackerBoardAttachment | null>(null);
  let choosing = $state(board === null);
  // The board shown is the one picked last and none of its cards showed
  // yet: a refusal lets it go.
  let unconfirmed = board !== null;
  // The last read of the board's computer started when the end was seen,
  // or when it was last renewed: only an answer read after it renews.
  let renewAfter: number | null = null;
  // The last read of the board's computer started when it was attached:
  // only an answer read after it moves the board to another listing.
  let attachedAfter = 0;
  const computer = $derived(board ? boardComputer(board.backend) : 'waiting');
  let ticket = $state.raw<OpenTicket | null>(null);
  let trail = $state.raw<readonly OpenTicket[]>([]);
  let ticketAttachment = $state.raw<TrackerTicketAttachment | null>(null);
  // A ticket asked for from outside the pane, while it waits on its
  // computer's boards (openRequested).
  let requested = $state.raw<TicketOpenRequest | null>(null);
  // The asked-for ticket the board switched to for it opens, in place of
  // the none a board change leaves.
  let arriving: OpenTicket | null = null;
  const placement: TicketPlacement = $derived(opts.besideFits() ? ticketPlacement('board') : 'over');
  // The one rule for whether the board is seen: the pane on screen, and no
  // ticket over it.
  const boardShown = $derived(opts.shown() && !(ticket && placement === 'over'));
  // The board on its computer: the ticket and its trail go when it changes,
  // and stay when only the project listing it does.
  const boardOnComputer = $derived(board ? trackerBoardOnComputer(board) : null);

  const stop = $effect.root(() => {
    $effect(() => {
      if (computer !== 'gone') return;
      unconfirmed = false;
      board = null;
    });

    $effect(() => {
      const ref = board;
      if (!ref || computer !== 'ready') {
        attachment = null;
        return;
      }
      // The subscribe carries whether the person sees the board, so a board
      // opened hidden reads nothing from the tracker until it shows.
      const held = untrack(() => attachTrackerBoard(ref, boardShown));
      attachment = held;
      renewAfter = null;
      attachedAfter = untrack(() => trackerBoardChoicesReadStarted(ref.backend));
      return () => held.release();
    });

    $effect(() => {
      const key = attachment?.key;
      const shown = boardShown;
      if (key) untrack(() => setTrackerBoardShown(key, shown));
    });

    $effect.pre(() => {
      void boardOnComputer;
      untrack(() => {
        ticket = arriving;
        arriving = null;
        trail = [];
      });
    });

    $effect(() => {
      const open = ticket;
      if (!open) {
        ticketAttachment = null;
        return;
      }
      const held = untrack(() => attachTrackerTicket(open.ref, opts.shown()));
      ticketAttachment = held;
      return () => held.release();
    });

    $effect(() => {
      const held = ticketAttachment;
      const shown = opts.shown();
      if (held) untrack(() => held.setShown(shown));
    });

    $effect(() => {
      const keys = [...trail, ...(ticket ? [ticket] : [])].map((t) => trackerTicketKey(t.ref));
      untrack(() => drafts.set(keys));
    });

    $effect(() => {
      const held = attachment;
      const ref = board;
      const state = held?.current;
      if (!held || !ref || !state) return;
      if (state.held) unconfirmed = false;
      if (unconfirmed) {
        // The subscribe answers for the board picked last.
        if (state.ended) {
          unconfirmed = false;
          board = null;
        }
        return;
      }
      const answeredBy = trackerBoardChoicesAnsweredBy(ref.backend);
      const rows = trackerBoardChoicesOf(ref.backend)?.rows ?? [];
      const listing = (c: BoardChoice) => c.connection === ref.connection && c.board === ref.board;
      if (answeredBy > attachedAfter && !rows.some((c) => listing(c) && c.project === ref.projectId)) {
        const moved = rows.find(listing);
        if (moved) untrack(() => moveTo(ref, choiceRef({ backend: ref.backend, choice: moved })));
        return;
      }
      if (!state.ended) {
        renewAfter = null;
        return;
      }
      renewAfter ??= untrack(() => trackerBoardChoicesReadStarted(ref.backend));
      if (answeredBy <= renewAfter) return;
      if (!rows.some((c) => listing(c) && c.project === ref.projectId && isBoardChoiceOpenable(c))) return;
      renewAfter = untrack(() => trackerBoardChoicesReadStarted(ref.backend));
      untrack(() => renewEndedTrackerBoard(held.key));
    });

    // A requested ticket opens over the board shown when that board is of
    // its computer and connection; otherwise, once that computer's boards
    // are known, the board switches to boardForTicket's and the ticket
    // opens with it (`arriving`, which the board change installs).
    $effect(() => {
      const request = requested;
      if (!request) return;
      const { ref } = request;
      const open: OpenTicket = { ref, seed: request.seed };
      const shown = board;
      if (shown && shown.backend === ref.backend && shown.connection === ref.connection) {
        untrack(() => {
          requested = null;
          trail = [];
          const held = ticket?.ref;
          if (held?.backend !== ref.backend || held.connection !== ref.connection || held.issue !== ref.issue) ticket = open;
        });
        return;
      }
      // A computer not connected opens the ticket when it connects, as a
      // board picked last does.
      const state = trackerBoardChoicesOf(ref.backend);
      if (state?.kind === 'reading' || state?.kind === 'offline') return;
      const next = boardForTicket(state?.rows ?? [], ref.backend, ref.connection, request.project);
      untrack(() => {
        requested = null;
        if (!next) {
          request.unplaced(state?.kind === 'failed' && !state.rows ? state.error : null);
          return;
        }
        unconfirmed = false;
        arriving = open;
        board = next;
      });
    });

    $effect(() => {
      if (board) {
        choosing = false;
        return;
      }
      const decision = openingBoard(trackerBoardChoiceComputers(), opts.openedFromProject);
      choosing = decision.waiting;
      if (decision.board) board = decision.board;
    });
  });

  // The board stays; its listing moved to another project. A save of the
  // board moves with it, so the next pane opens it too.
  function moveTo(from: TrackerBoardRef, to: TrackerBoardRef): void {
    if (sameBoard(pickedBoard(), from)) savePick(to);
    board = to;
  }

  return {
    get board() { return board; },
    get attachment() { return attachment; },
    get choosing() { return choosing; },
    get ticket() { return ticket; },
    get ticketAttachment() { return ticketAttachment; },
    get placement() { return placement; },
    get boardShown() { return boardShown; },
    get trail() { return trail.length; },
    openTicket(issue, seed, from) {
      if (!board || ticket?.ref.issue === issue) return;
      const next: OpenTicket = { ref: { backend: board.backend, connection: board.connection, issue }, seed };
      trail = from === 'ticket' && ticket ? [...trail, ticket].slice(-MAX_TICKET_TRAIL) : [];
      ticket = next;
    },
    openRequested(request) {
      requested = request;
    },
    stepBack() {
      if (trail.length === 0) {
        ticket = null;
        return;
      }
      ticket = trail[trail.length - 1];
      trail = trail.slice(0, -1);
    },
    closeTicket() {
      ticket = null;
      trail = [];
    },
    pick(ref) {
      savePick(ref);
      unconfirmed = false;
      if (sameBoard(board, ref)) {
        if (attachment) renewEndedTrackerBoard(attachment.key);
        return;
      }
      board = { backend: ref.backend, projectId: ref.projectId, connection: ref.connection, board: ref.board };
    },
    destroy() {
      stop();
      releaseChoices();
      drafts.release();
    },
  };
}

export function choiceRef({ backend, choice }: ListedBoardChoice): TrackerBoardRef {
  return { backend, projectId: choice.project, connection: choice.connection, board: choice.board };
}

export function resetIssuesPaneForTest(): void {
  hiddenFields = new Map();
  compactColumns.clear();
  placements = {};
  sections = new Map();
  commentOrder = 'oldest';
  ticketOpenRequest = null;
}
