// The Issues pane's view state (docs/specs/issues.md, "The Issues pane"):
// the board it shows, which card fields each board shows (Display), Only
// mine, and the column a compact screen shows. The board, Display and Only
// mine belong to this client (frontendStorage), so another window of it
// follows them and no computer is asked; the compact column lasts while the
// app runs. The subscription itself is trackerBoard.svelte.ts's, the boards
// to pick from trackerBoardChoices.svelte.ts's.

import { SvelteMap } from 'svelte/reactivity';
import { onFrontendValueChanged, readFrontendValue, writeFrontendValue } from './frontendStorage';
import { trackerBoardOnComputer, type TrackerBoardRef } from './trackerBoard.svelte';
import type { ListedBoardChoice } from './trackerBoardChoices.svelte';

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

let shownBoard = $state.raw<TrackerBoardRef | null>(readBoard());
let hiddenFields = $state.raw(readHiddenFields());
let onlyMine = $state(readFrontendValue(ONLY_MINE_KEY) === true);
const compactColumns = new SvelteMap<string, number>();

onFrontendValueChanged(BOARD_KEY, () => { shownBoard = readBoard(); });
onFrontendValueChanged(HIDDEN_FIELDS_KEY, () => { hiddenFields = readHiddenFields(); });
onFrontendValueChanged(ONLY_MINE_KEY, () => { onlyMine = readFrontendValue(ONLY_MINE_KEY) === true; });

/** The board this client showed last; null before it showed one. */
export function lastShownBoard(): TrackerBoardRef | null {
  return shownBoard;
}

export function sameBoard(a: TrackerBoardRef | null, b: TrackerBoardRef | null): boolean {
  return a === b || (!!a && !!b && a.backend === b.backend && a.projectId === b.projectId && a.connection === b.connection && a.board === b.board);
}

export function showBoard(ref: TrackerBoardRef): void {
  if (sameBoard(shownBoard, ref)) return;
  const next = { backend: ref.backend, projectId: ref.projectId, connection: ref.connection, board: ref.board };
  if (writeFrontendValue(BOARD_KEY, next)) shownBoard = next;
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

export function onlyMineOn(): boolean {
  return onlyMine;
}

export function setOnlyMine(on: boolean): void {
  if (writeFrontendValue(ONLY_MINE_KEY, on)) onlyMine = on;
}

/** The column a compact screen shows of a board, null before one was
 * picked this session. */
export function compactColumn(ref: TrackerBoardRef): number | null {
  return compactColumns.get(trackerBoardOnComputer(ref)) ?? null;
}

export function setCompactColumn(ref: TrackerBoardRef, column: number): void {
  compactColumns.set(trackerBoardOnComputer(ref), column);
}

/**
 * The board the pane opens on: the one this client showed last while some
 * project still lists it; else the board that opens first of the project
 * the pane was opened from; else that of the first project, in `projectOrder`,
 * that lists one; null when no project lists a board.
 */
export function openingBoard(
  choices: readonly ListedBoardChoice[],
  last: TrackerBoardRef | null,
  openedFromProject: string | null,
  projectOrder: (backend: string, projectId: string) => number,
): TrackerBoardRef | null {
  const refs = choices.map(choiceRef);
  const kept = refs.find((ref) => sameBoard(ref, last));
  if (kept) return kept;
  const firstOf = (i: number) => {
    const { backend, choice } = choices[i];
    // The first listed is the one that opens first when none is marked.
    const ofProject = choices.flatMap((c, j) => (c.backend === backend && c.choice.project === choice.project ? [j] : []));
    return refs[ofProject.find((j) => choices[j].choice.opens_first) ?? ofProject[0]];
  };
  const from = choices.findIndex((c) => c.choice.project === openedFromProject);
  if (from >= 0) return firstOf(from);
  if (choices.length === 0) return null;
  let best = 0;
  for (let i = 1; i < choices.length; i++) {
    if (projectOrder(choices[i].backend, choices[i].choice.project) < projectOrder(choices[best].backend, choices[best].choice.project)) best = i;
  }
  return firstOf(best);
}

export function choiceRef({ backend, choice }: ListedBoardChoice): TrackerBoardRef {
  return { backend, projectId: choice.project, connection: choice.connection, board: choice.board };
}

export function resetIssuesPaneForTest(): void {
  shownBoard = null;
  hiddenFields = new Map();
  onlyMine = false;
  compactColumns.clear();
}
