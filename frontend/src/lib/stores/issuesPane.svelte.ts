// The Issues pane's view state (docs/specs/issues.md, "The Issues pane"):
// the board a pane shows and the ticket open over or beside it
// (`createBoardSelection`), which card fields each board shows (Display),
// Only mine, the column a compact screen shows, which Backlog sections are
// folded, what a new ticket was last made with on each board, and the
// ticket's remembered choices: over or beside, which sections are open,
// and the comment order. The board a person picked last and Only mine are
// saved for this client (frontendStorage) and seed the next pane that
// opens; Display and the folds follow another window of the client as
// they change; the compact column and a new ticket's choices last while
// the app runs. No computer is asked for any of it. The
// subscriptions themselves are trackerBoard.svelte.ts's and
// trackerTicket.svelte.ts's, the boards to pick from
// trackerBoardChoices.svelte.ts's.

import { untrack } from 'svelte';
import { SvelteMap } from 'svelte/reactivity';
import { getAttachedBackends } from './attachedBackends.svelte';
import { onFrontendValueChanged, readFrontendValue, writeFrontendValue } from './frontendStorage';
import {
  attachTrackerBoard,
  holdTrackerBoardPlan,
  renewEndedTrackerBoard,
  setTrackerBoardShown,
  trackerBoardOnComputer,
  type TrackerBoardAttachment,
  type TrackerBoardRef,
} from './trackerBoard.svelte';
import { attachTrackerTicket, trackerTicketKey, type TrackerTicketAttachment, type TrackerTicketRef } from './trackerTicket.svelte';
import { closeTicketPane, sameTicketRef, setTicketPaneTicket, takeTicketPaneHandoff, ticketPaneRecord } from './ticketPanes.svelte';
import { createOpenFocusRequest, type OpenFocusRequest } from '../utils/openFocusRequest.svelte';
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
const SCREEN_KEY = 'issues-screen';
const SECTIONS_KEY = 'issues-ticket-folds';
const BACKLOG_FOLDS_KEY = 'issues-backlog-folds';
const COMMENT_ORDER_KEY = 'issues-ticket-comment-order';
const TIME_LAYOUT_KEY = 'issues-time-layout';
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

/** How the Time screen lays out a week. */
export type TimeLayout = 'day' | 'ticket';

/** The layout this client last picked: what a Time screen opens with. */
export function savedTimeLayout(): TimeLayout {
  return readFrontendValue(TIME_LAYOUT_KEY) === 'ticket' ? 'ticket' : 'day';
}

export function saveTimeLayout(layout: TimeLayout): void {
  writeFrontendValue(TIME_LAYOUT_KEY, layout);
}

/** The column a compact screen shows of a board, null before one was
 * picked this session. */
export function compactColumn(ref: TrackerBoardRef): number | null {
  return compactColumns.get(trackerBoardOnComputer(ref)) ?? null;
}

export function setCompactColumn(ref: TrackerBoardRef, column: number): void {
  compactColumns.set(trackerBoardOnComputer(ref), column);
}

/** The pane's screens: the tabs of its top bar. Board and Backlog show
 * the board; Time shows the person's week. */
export type IssuesScreen = 'board' | 'backlog' | 'time';

export const ISSUES_SCREENS: readonly { readonly screen: IssuesScreen; readonly label: string }[] = [
  { screen: 'board', label: 'Board' },
  { screen: 'backlog', label: 'Backlog' },
  { screen: 'time', label: 'Time' },
];

function isScreen(v: unknown): v is IssuesScreen {
  return v === 'board' || v === 'backlog' || v === 'time';
}

/** Whether a screen shows the board. */
export function showsBoard(screen: IssuesScreen): boolean {
  return screen === 'board' || screen === 'backlog';
}

/** The screen this client last switched to: what a pane opens on. */
export function savedScreen(): IssuesScreen {
  const v = readFrontendValue(SCREEN_KEY);
  return isScreen(v) ? v : 'board';
}

/** Where a screen shows a ticket: over the list, or beside it. */
export type TicketPlacement = 'over' | 'beside';

function isPlacement(v: unknown): v is TicketPlacement {
  return v === 'over' || v === 'beside';
}

function readPlacements(): Partial<Record<IssuesScreen, TicketPlacement>> {
  const v = readFrontendValue(PLACEMENT_KEY) as Record<string, unknown> | null;
  if (!v || typeof v !== 'object') return {};
  const out: Partial<Record<IssuesScreen, TicketPlacement>> = {};
  for (const { screen } of ISSUES_SCREENS) {
    const p = v[screen];
    if (isPlacement(p)) out[screen] = p;
  }
  return out;
}

let placements = $state.raw(readPlacements());

// A ticket opens over the board and beside every list (docs/specs/issues.md, "Pane").
const DEFAULT_PLACEMENT: Readonly<Record<IssuesScreen, TicketPlacement>> = { board: 'over', backlog: 'beside', time: 'beside' };

/** Where the screen shows a ticket when the pane is wide enough for
 * beside; the screen's default until the person chose. Reactive. */
export function ticketPlacement(screen: IssuesScreen): TicketPlacement {
  return placements[screen] ?? DEFAULT_PLACEMENT[screen];
}

export function setTicketPlacement(screen: IssuesScreen, placement: TicketPlacement): void {
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

/** Each Backlog section's fold as the person set it, by board on its
 * computer and list (`trackerBoardModel.ts#sectionList`), oldest first. */
function readBacklogFolds(): ReadonlyMap<string, boolean> {
  const v = readFrontendValue(BACKLOG_FOLDS_KEY);
  if (!Array.isArray(v)) return new Map();
  const out = new Map<string, boolean>();
  for (const entry of v.slice(-MAX_SECTIONS_REMEMBERED)) {
    if (Array.isArray(entry) && entry.length === 2 && isString(entry[0]) && typeof entry[1] === 'boolean') out.set(entry[0], entry[1]);
  }
  return out;
}

let backlogFolds = $state.raw(readBacklogFolds());
onFrontendValueChanged(BACKLOG_FOLDS_KEY, () => { backlogFolds = readBacklogFolds(); });

function backlogFoldKey(ref: TrackerBoardRef, list: string): string {
  return `${trackerBoardOnComputer(ref)}\n${list}`;
}

/** Whether a Backlog section of the board is open: as the person left it,
 * else `opens` (an active sprint opens folded). Reactive. */
export function backlogSectionOpen(ref: TrackerBoardRef, list: string, opens: boolean): boolean {
  return backlogFolds.get(backlogFoldKey(ref, list)) ?? opens;
}

export function setBacklogSectionOpen(ref: TrackerBoardRef, list: string, open: boolean): void {
  const key = backlogFoldKey(ref, list);
  const next = new Map(backlogFolds);
  // Re-inserted so the section changed last is kept longest.
  next.delete(key);
  next.set(key, open);
  while (next.size > MAX_SECTIONS_REMEMBERED) next.delete(next.keys().next().value!);
  if (writeFrontendValue(BACKLOG_FOLDS_KEY, [...next])) backlogFolds = next;
}

/** What a new ticket was last made with on a board this session: its
 * project (on a board of several) and its type. */
export interface CreateChoice {
  /** The project, as its form named it, so a later form names it with no
   * read. */
  readonly project?: { readonly id: string; readonly name: string };
  readonly type?: string;
}

const createChoices = new SvelteMap<string, CreateChoice>();

/** What the person last made a ticket with on the board this session;
 * reactive. */
export function lastCreateChoice(ref: TrackerBoardRef): CreateChoice {
  return createChoices.get(trackerBoardOnComputer(ref)) ?? {};
}

export function setLastCreateChoice(ref: TrackerBoardRef, choice: CreateChoice): void {
  const key = trackerBoardOnComputer(ref);
  createChoices.set(key, { ...createChoices.get(key), ...choice });
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
  /** The project the opener named (a thread's, a pull request's); null
   * for a ticket opened from the board, whose project is the board's. */
  readonly project: string | null;
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
  /** The ticket open over or beside the board, its subscription and the
   * tickets Back steps through. */
  readonly tickets: TicketTrail;
  /** The ticket open over or beside the board; null for none. Reactive. */
  readonly ticket: OpenTicket | null;
  /** The screen the pane shows: the Board, the Backlog or Time. Reactive. */
  readonly screen: IssuesScreen;
  /** Where the ticket shows now: beside only when the pane fits it and the
   * screen's placement is beside. Reactive. */
  readonly placement: TicketPlacement;
  /** Whether the person sees the board: the pane is on screen, shows the
   * Board or the Backlog and no ticket covers it. The board's subscription
   * votes this. Reactive. */
  readonly boardShown: boolean;
  /** Whether the person sees the Time screen, by the same rule. Its week
   * is read only while it is. Reactive. */
  readonly timeShown: boolean;
  /** The person picked `ref`: shown and saved for the next pane. A pick of
   * the board shown while it has ended subscribes it again. */
  pick(ref: TrackerBoardRef): void;
  /** Switches the screen: saved for the next pane; the ticket open and
   * its trail close. */
  setScreen(screen: IssuesScreen): void;
  /** A person opens issue `issue` of the board's connection: from the
   * screen's list, or from the ticket open now, which Back then returns
   * to. */
  openTicket(issue: string, seed: TicketSeed, from: 'list' | 'ticket'): void;
  /** Opens a ticket asked for from outside the pane, on the Board or
   * Backlog screen, whichever shows: over the board shown when it is of the ticket's computer and
   * connection, else over the board `boardForTicket` picks once that
   * computer's boards are read, which it shows without saving it as the
   * person's pick. A pane on Time switches to the Board without saving
   * that either. No such board runs the request's `unplaced`. The ticket
   * already open stays open, and the project the request names becomes
   * its project unless the person picked one (`TicketTrail.project`). */
  openRequested(request: TicketOpenRequest): void;
  /** Back: the ticket this one was opened from, else closes it. */
  stepBack(): void;
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

/** Where a tracker's computer stands for a pane: `waiting` until the page
 * knows its grants, `gone` when it is not attached or refuses tracker:read
 * (it can never answer), else `ready`. Reactive. */
export function trackerComputer(backend: string): 'waiting' | 'gone' | 'ready' {
  if (!pageGrantsKnownTracked()) return 'waiting';
  if (!getAttachedBackends().some((entry) => entry.id === backend)) return 'gone';
  const grant = scopeGrant('tracker:read', backend);
  return grant === 'granted' ? 'ready' : grant === 'refused' ? 'gone' : 'waiting';
}

/** Where a ticket trail keeps the ticket it shows and the tickets Back
 * steps through, and the focus its opens ask for: the Issues pane's own
 * (`localTicketTrailState`) or a ticket pane's record
 * (`ticketPaneTrailState`). */
export interface TicketTrailState {
  /** Reactive. */
  readonly ticket: OpenTicket | null;
  /** Reactive; oldest first. */
  readonly trail: readonly OpenTicket[];
  set(ticket: OpenTicket, trail: readonly OpenTicket[]): void;
  /** No ticket shown here: the Issues pane shows its list, a ticket pane
   * closes. */
  close(): void;
  readonly focus: OpenFocusRequest<OpenTicket>;
  /** What held the ticket until this trail subscribed it, taken. */
  takeHandoff(): (() => void) | null;
}

/** A pane's ticket and the tickets it was opened from, and the
 * subscription that shows it. The one owner of the open, Back and Close
 * rules, for the Issues pane and for a ticket pane alike. */
export interface TicketTrail {
  /** The ticket shown; null for none. Reactive. */
  readonly ticket: OpenTicket | null;
  /** The ticket shown and its subscription, set together, so what draws
   * one never draws the other's; null with no ticket or while its
   * computer is not ready. Reactive. */
  readonly view: { readonly open: OpenTicket; readonly attachment: TrackerTicketAttachment } | null;
  /** Where the ticket's computer stands; null with no ticket. Reactive. */
  readonly computer: 'waiting' | 'gone' | 'ready' | null;
  /** How many tickets Back steps through. Reactive. */
  readonly depth: number;
  /** The ticket shown and those on the trail. Reactive. */
  readonly held: readonly OpenTicket[];
  /** The pane is on screen now, so the person sees the ticket it shows
   * (`TicketTrailOptions.shown`). Reactive. */
  readonly shown: boolean;
  /** The project of the ticket shown, which its Ask and Start work make
   * their thread in and whose setup holds its Start work defaults: the
   * one the person picked for it, else the one its last opener that named
   * a project named, else `TicketTrailOptions.defaultProject`'s. Null with
   * no ticket or none known. Reactive. */
  readonly project: string | null;
  /** The person picked `project` for the ticket shown; it holds while that
   * open is shown, over any later opener's. */
  setProject(project: string): void;
  /** Whether the open shown asked for the focus and nothing ended the
   * ask (utils/openFocusRequest.svelte.ts). Reactive. */
  readonly focusAsked: boolean;
  /** The surface took the focus the open asked for. */
  focusTaken(): void;
  /** A person opens `ref`: from a list (the trail starts over) or from the
   * ticket shown, which Back then returns to. It asks for the focus; an
   * open of the ticket shown asks again and changes nothing else. */
  open(ref: TrackerTicketRef, seed: TicketSeed, from: 'list' | 'ticket'): void;
  /** Shows `open`, or nothing, with no trail and asking no focus: an open
   * no person made here (a thread's link, a board change). The ticket
   * shown stays when it is the same ticket, and takes the project `open`
   * names (`project`). */
  show(open: OpenTicket | null): void;
  /** A person's Back to the ticket this one was opened from, which asks
   * for the focus. False when the trail is empty. */
  stepBack(): boolean;
  /** No ticket and no trail, as its host closes a ticket
   * (`TicketTrailState.close`). */
  close(): void;
  destroy(): void;
}

/** The Issues pane's own state for its trail. */
export function localTicketTrailState(): TicketTrailState {
  let ticket = $state.raw<OpenTicket | null>(null);
  let trail = $state.raw<readonly OpenTicket[]>([]);
  return {
    get ticket() { return ticket; },
    get trail() { return trail; },
    set(next, nextTrail) {
      ticket = next;
      trail = nextTrail;
    },
    close() {
      ticket = null;
      trail = [];
    },
    focus: createOpenFocusRequest<OpenTicket>(),
    takeHandoff: () => null,
  };
}

/** A ticket pane's record as its trail's state. */
export function ticketPaneTrailState(paneId: string): TicketTrailState {
  const record = ticketPaneRecord(paneId);
  if (!record) throw new Error(`ticketPaneTrailState: ${paneId} is not a ticket pane`);
  const { focus } = record;
  return {
    get ticket() { return ticketPaneRecord(paneId)?.ticket ?? null; },
    get trail() { return ticketPaneRecord(paneId)?.trail ?? []; },
    set(next, nextTrail) {
      setTicketPaneTicket(paneId, next, nextTrail);
    },
    close() {
      closeTicketPane(paneId);
    },
    focus,
    takeHandoff: () => takeTicketPaneHandoff(paneId),
  };
}

export interface TicketTrailOptions {
  readonly state: TicketTrailState;
  /** Whether the pane is on screen now; reactive. */
  readonly shown: () => boolean;
  /** The project of a ticket no word names (`TicketTrail.project`): the
   * Issues pane's board's. A ticket pane has none: a ticket moved there
   * carries the project it had, and one opened there with none is the
   * person's to pick. Reactive. */
  readonly defaultProject?: () => string | null;
}

/** Holds the subscription of the ticket `state` shows until `destroy`;
 * create it in a component's setup. */
export function createTicketTrail({ state, shown, defaultProject = () => null }: TicketTrailOptions): TicketTrail {
  let view = $state.raw<TicketTrail['view']>(null);
  // An unchanged ticket stops here, so a record replaced for its trail
  // alone subscribes nothing again.
  const ticket = $derived(state.ticket);
  const computer = $derived(ticket ? trackerComputer(ticket.ref.backend) : null);
  // The ticket's project keeps these true. Three words name it, and the
  // first that holds wins: the project the person picked in Work's picker
  // for this open; else the one the last opener that named a project
  // named (its OpenTicket's, or a later opener's that asked for the ticket
  // already shown); else the default. A pick holds while its open is the
  // one shown: no later opener replaces it. An opener that names no
  // project leaves the word before it. A later opener's word changes only
  // this: the open, its subscription and its screen stay. The word
  // belongs to the open it was given for, so a new open, Back and Close
  // fall back to their own.
  let projectWord = $state.raw<{ readonly open: OpenTicket; readonly project: string; readonly picked: boolean } | null>(null);
  const project = $derived(!ticket ? null : projectWord?.open === ticket ? projectWord.project : ticket.project ?? defaultProject());

  const stop = $effect.root(() => {
    $effect(() => {
      const open = ticket;
      if (!open || computer !== 'ready') {
        view = null;
        // A computer that can never answer shows nothing: what held the
        // ticket for this trail lets go.
        if (computer === 'gone') untrack(() => state.takeHandoff()?.());
        return;
      }
      const attachment = untrack(() => attachTrackerTicket(open.ref, shown()));
      view = { open, attachment };
      untrack(() => state.takeHandoff()?.());
      return () => attachment.release();
    });

    $effect(() => {
      const held = view?.attachment;
      const on = shown();
      if (held) untrack(() => held.setShown(on));
    });
  });

  return {
    get ticket() { return ticket; },
    get view() { return view; },
    get computer() { return computer; },
    get depth() { return state.trail.length; },
    get shown() { return shown(); },
    get project() { return project; },
    setProject(next) {
      if (ticket) projectWord = { open: ticket, project: next, picked: true };
    },
    get held() { return state.ticket ? [...state.trail, state.ticket] : state.trail; },
    get focusAsked() { return state.focus.askedFor(view?.open ?? null); },
    focusTaken() {
      state.focus.end();
    },
    open(ref, seed, from) {
      const current = state.ticket;
      if (current && sameTicketRef(current.ref, ref)) {
        state.focus.ask(current);
        return;
      }
      const next: OpenTicket = { ref, seed, project: null };
      state.set(next, from === 'ticket' && current ? [...state.trail, current].slice(-MAX_TICKET_TRAIL) : []);
      state.focus.ask(next);
    },
    show(open) {
      state.focus.end();
      const current = state.ticket;
      if (!open) {
        state.close();
        return;
      }
      if (!current || !sameTicketRef(current.ref, open.ref)) {
        state.set(open, []);
        return;
      }
      state.set(current, []);
      if (open.project !== null && !(projectWord?.open === current && projectWord.picked)) {
        projectWord = { open: current, project: open.project, picked: false };
      }
    },
    stepBack() {
      const trail = state.trail;
      if (trail.length === 0) return false;
      const back = trail[trail.length - 1];
      state.set(back, trail.slice(0, -1));
      state.focus.ask(back);
      return true;
    },
    close() {
      state.focus.end();
      state.close();
    },
    destroy() {
      stop();
      state.focus.end();
    },
  };
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
  const computer = $derived(board ? trackerComputer(board.backend) : 'waiting');
  const tickets = createTicketTrail({ state: localTicketTrailState(), shown: opts.shown, defaultProject: () => board?.projectId ?? null });
  const ticket = $derived(tickets.ticket);
  // A ticket asked for from outside the pane, while it waits on its
  // computer's boards (openRequested).
  let requested = $state.raw<TicketOpenRequest | null>(null);
  // The asked-for ticket the board switched to for it opens, in place of
  // the none a board change leaves.
  let arriving: OpenTicket | null = null;
  let screen = $state<IssuesScreen>(savedScreen());
  const placement: TicketPlacement = $derived(opts.besideFits() ? ticketPlacement(screen) : 'over');
  // The one rule for whether a screen is seen: the pane on screen, showing
  // it, and no ticket over it.
  const listShown = $derived(opts.shown() && !(ticket && placement === 'over'));
  const boardShown = $derived(listShown && showsBoard(screen));
  const timeShown = $derived(listShown && screen === 'time');
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

    // The Backlog's sections are held while the pane is on Backlog, shown
    // or not, so a ticket opened over it and closed reads none again.
    $effect(() => {
      const key = attachment?.key;
      if (!key || screen !== 'backlog') return;
      return untrack(() => holdTrackerBoardPlan(key));
    });

    $effect.pre(() => {
      void boardOnComputer;
      untrack(() => {
        tickets.show(arriving);
        arriving = null;
      });
    });

    $effect(() => {
      const keys = tickets.held.map((t) => trackerTicketKey(t.ref));
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

    // A requested ticket opens on the Board or Backlog screen, over the board shown
    // when that board is of its computer and connection; otherwise, once
    // that computer's boards are known, the board switches to
    // boardForTicket's and the ticket opens with it (`arriving`, which the
    // board change installs). The switch from Time is not saved: the next
    // pane opens on the screen the person picked.
    $effect(() => {
      const request = requested;
      if (!request) return;
      const { ref } = request;
      const open: OpenTicket = { ref, seed: request.seed, project: request.project };
      const shown = board;
      if (shown && shown.backend === ref.backend && shown.connection === ref.connection) {
        untrack(() => {
          requested = null;
          if (!showsBoard(screen)) screen = 'board';
          tickets.show(open);
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
        if (!showsBoard(screen)) screen = 'board';
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
    get tickets() { return tickets; },
    get ticket() { return ticket; },
    get screen() { return screen; },
    get placement() { return placement; },
    get boardShown() { return boardShown; },
    get timeShown() { return timeShown; },
    setScreen(next) {
      writeFrontendValue(SCREEN_KEY, next);
      if (next === screen) return;
      screen = next;
      tickets.close();
    },
    openTicket(issue, seed, from) {
      if (!board) return;
      tickets.open({ backend: board.backend, connection: board.connection, issue }, seed, from);
    },
    openRequested(request) {
      requested = request;
    },
    stepBack() {
      if (!tickets.stepBack()) tickets.close();
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
      tickets.destroy();
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
  backlogFolds = new Map();
  createChoices.clear();
  commentOrder = 'oldest';
  ticketOpenRequest = null;
}
