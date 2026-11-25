// The Issues pane's view state (docs/specs/issues.md, "The Issues pane"):
// the board a pane shows (`createBoardSelection`), which card fields each
// board shows (Display), Only mine, and the column a compact screen shows.
// The board a person picked last and Only mine are saved for this client
// (frontendStorage) and seed the next pane that opens; Display follows
// another window of the client as it changes; the compact column lasts
// while the app runs. No computer is asked for any of it. The subscription
// itself is trackerBoard.svelte.ts's, the boards to pick from
// trackerBoardChoices.svelte.ts's.

import { untrack } from 'svelte';
import { SvelteMap } from 'svelte/reactivity';
import { onFrontendValueChanged, readFrontendValue, writeFrontendValue } from './frontendStorage';
import {
  attachTrackerBoard,
  renewEndedTrackerBoard,
  setTrackerBoardShown,
  trackerBoardOnComputer,
  type TrackerBoardAttachment,
  type TrackerBoardRef,
} from './trackerBoard.svelte';
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
// Boards whose Display was changed, newest last; the oldest goes past this.
const MAX_BOARDS_REMEMBERED = 200;

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

/** What the opening rule decided: a board, or none, and whether it still
 * waits on a computer's boards. */
export interface OpeningDecision {
  readonly board: TrackerBoardRef | null;
  readonly waiting: boolean;
}

/**
 * The board a pane opens on when the person has picked none, from what the
 * computers answered (`computers`, in attach order): the board that opens
 * first of the project the pane was opened from, else that project's first
 * openable one, as soon as its computer answered; else, once every computer
 * has answered or could not be read, the same of the first project in
 * sidebar order (`order`, read only then) that lists an openable board;
 * else none. A board that cannot be opened (`isBoardChoiceOpenable`) is
 * never chosen.
 */
export function openingBoard(
  computers: readonly { readonly backend: string; readonly state: ComputerChoices }[],
  openedFromProject: string | null,
  order: () => ReadonlyMap<string, number>,
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
  const positions = order();
  const at = (project: string) => positions.get(project) ?? Number.POSITIVE_INFINITY;
  let best: TrackerBoardRef | null = null;
  for (const c of choices) {
    if (!isBoardChoiceOpenable(c.choice)) continue;
    if (best && at(c.choice.project) >= at(best.projectId)) continue;
    best = firstOf(c.backend, c.choice.project);
  }
  return { board: best, waiting: false };
}

/** The board a pane shows, and the subscription that shows it. */
export interface BoardSelection {
  /** The board shown; null while the opening rule waits or when it found
   * none. Reactive. */
  readonly board: TrackerBoardRef | null;
  /** Its subscription, null with no board. Reactive. */
  readonly attachment: TrackerBoardAttachment | null;
  /** The opening rule is waiting on a computer's boards. Reactive. */
  readonly choosing: boolean;
  /** The person picked `ref`: shown and saved for the next pane. A pick of
   * the board shown while it has ended subscribes it again. */
  pick(ref: TrackerBoardRef): void;
  destroy(): void;
}

export interface BoardSelectionOptions {
  /** The project of the thread the pane was opened from. */
  readonly openedFromProject: string | null;
  /** Whether the pane is on screen now; reactive. */
  readonly shown: () => boolean;
  /** Each project's position in the sidebar as it shows now. */
  readonly projectOrder: () => ReadonlyMap<string, number>;
}

/**
 * One pane's board, chosen once: the person's pick, else the board picked
 * last (opened at once, beside the read of the boards), else the opening
 * rule. Nothing else changes it: another window, a later answer, a board
 * that ends. Its computer is the authority on whether it opens: when it
 * refuses or ends the board picked last before the pane showed any of its
 * cards, the selection lets it go and the opening rule chooses. An ended
 * board is subscribed again once an answer of its computer read after the
 * end lists it as openable (a connection turned back on). Holds the board
 * choices and the subscription until `destroy`; create it in a component's
 * setup.
 */
export function createBoardSelection(opts: BoardSelectionOptions): BoardSelection {
  const releaseChoices = acquireTrackerBoardChoices();
  let board = $state.raw<TrackerBoardRef | null>(pickedBoard());
  let attachment = $state.raw<TrackerBoardAttachment | null>(null);
  let choosing = $state(board === null);
  // The board shown is the one picked last and none of its cards showed
  // yet: a refusal lets it go.
  let unconfirmed = board !== null;
  // The last read of the board's computer started when the end was seen,
  // or when it was last renewed: only an answer read after it renews.
  let renewAfter: number | null = null;

  const stop = $effect.root(() => {
    $effect(() => {
      const ref = board;
      if (!ref) {
        attachment = null;
        return;
      }
      // The subscribe carries whether the pane shows the board, so a board
      // opened hidden reads nothing from the tracker until it shows.
      const held = untrack(() => attachTrackerBoard(ref, opts.shown()));
      attachment = held;
      renewAfter = null;
      return () => held.release();
    });

    $effect(() => {
      const key = attachment?.key;
      const shown = opts.shown();
      if (key) untrack(() => setTrackerBoardShown(key, shown));
    });

    $effect(() => {
      const held = attachment;
      const ref = board;
      const state = held?.current;
      if (!held || !ref || !state) return;
      if (state.held) unconfirmed = false;
      if (!state.ended) {
        renewAfter = null;
        return;
      }
      if (unconfirmed) {
        unconfirmed = false;
        board = null;
        return;
      }
      renewAfter ??= untrack(() => trackerBoardChoicesReadStarted(ref.backend));
      if (trackerBoardChoicesAnsweredBy(ref.backend) <= renewAfter) return;
      const rows = trackerBoardChoicesOf(ref.backend)?.rows ?? [];
      if (!rows.some((c) => sameBoard(choiceRef({ backend: ref.backend, choice: c }), ref) && isBoardChoiceOpenable(c))) return;
      renewAfter = untrack(() => trackerBoardChoicesReadStarted(ref.backend));
      untrack(() => renewEndedTrackerBoard(held.key));
    });

    $effect(() => {
      if (board) {
        choosing = false;
        return;
      }
      const decision = openingBoard(trackerBoardChoiceComputers(), opts.openedFromProject, () => untrack(opts.projectOrder));
      choosing = decision.waiting;
      if (decision.board) board = decision.board;
    });
  });

  return {
    get board() { return board; },
    get attachment() { return attachment; },
    get choosing() { return choosing; },
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
    },
  };
}

export function choiceRef({ backend, choice }: ListedBoardChoice): TrackerBoardRef {
  return { backend, projectId: choice.project, connection: choice.connection, board: choice.board };
}

export function resetIssuesPaneForTest(): void {
  hiddenFields = new Map();
  compactColumns.clear();
}
