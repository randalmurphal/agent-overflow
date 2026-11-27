// What the client holds of a tracker board, and the pure rules that change
// it: the patch rule, a further page, the grouping of a sprint's rows into
// the board's columns, and the Only mine and Search match. Which sections
// the Board screen reads is the board's computer's (`BoardView.screen`),
// and so are the Backlog screen's (`BoardView.plan`), held while a pane
// shows Backlog (`withPlan`). No state lives here; the board store
// (trackerBoard.svelte.ts) owns it and replaces a held board whole.
//
// The patch rule is the backend's (the `BoardPatch` comment in
// internal/trackerapp/patch.go), whose reference is `applyPatch` in
// internal/trackerapp/patch_model_test.go. Both replay one recorded run,
// internal/trackerapp/testdata/board_patch_cases.json.

import type {
  BoardPatch,
  BoardTotals,
  BoardView,
  CardView,
  ColumnView,
  MoveView,
  PaceView,
  RowView,
  Section,
  SectionCursor,
  SectionPage,
  SectionTotal,
  SprintView,
  TotalsAt,
} from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { markGroup } from './statusMarkGroup';

/** A section as the client holds it: the rows of the pages it loaded, in
 * board order, and where the next page starts (null: loaded whole).
 * `unread` marks a Board screen section the view carried no page of (one
 * past the 20 a view reads): it holds no row, and its first page is owed. */
export interface HeldSection {
  readonly section: Section;
  readonly rows: readonly RowView[];
  readonly next: SectionCursor | null;
  readonly unread?: true;
}

/** Every section's total at the board's totals revision, and its
 * finished sprints' pace at it (null for none). A sprint or column a map
 * lacks holds no row. */
export interface HeldTotals {
  readonly revision: number;
  readonly all: SectionTotal;
  readonly backlog: SectionTotal;
  readonly sprints: ReadonlyMap<string, SectionTotal>;
  readonly columns: ReadonlyMap<number, SectionTotal>;
  readonly pace: PaceView | null;
}

/** The board's structure as the screen draws it. */
export interface BoardShape {
  readonly id: string;
  readonly name: string;
  /** The tracker project the board shows; empty for a board of several. */
  readonly project: string;
  readonly plansSprints: boolean;
  /** A card's own estimate: none, points, or time (seconds). */
  readonly unit: string;
  /** The unit the project sums the board's work in (`BoardView.counts`):
   * `unit`, or none when the project counts tickets. Every total, the
   * pace and a person's load read it; a card's estimate reads `unit`. */
  readonly counts: string;
  readonly columns: readonly ColumnView[];
  readonly sprints: readonly SprintView[];
  /** A card's address less its key; empty when cards carry their own
   * (`cardURL`). */
  readonly issueURL: string;
}

/** What the client holds of a board: its shape, lifetime and revision,
 * the version of the project's status groups it was read with (empty for
 * none), each status those groups put in another group than the tracker's
 * (`regrouped`) and those they leave out of counts, the totals with
 * the pace, the sections the Board and Backlog screens show (`screen`,
 * `plan`), and the sections it reads: the Board screen's, and the Backlog
 * screen's while a pane shows Backlog. */
export interface HeldBoard {
  readonly shape: BoardShape;
  readonly lifetime: number;
  readonly revision: number;
  readonly groups: string;
  readonly regrouped: ReadonlyMap<string, string>;
  readonly leftOut: ReadonlySet<string>;
  readonly totals: HeldTotals;
  readonly screen: readonly Section[];
  readonly plan: readonly Section[];
  readonly sections: readonly HeldSection[];
}

const NO_ROWS: SectionTotal = { rows: 0, estimate: 0, unestimated: 0 };

/** A total, none when the map lacks it. */
export function totalOf<K>(map: ReadonlyMap<K, SectionTotal>, key: K): SectionTotal {
  return map.get(key) ?? NO_ROWS;
}

/** The board a full read answered, its pages interned through `intern`,
 * with each Board screen section it carried no page of held unread, in the
 * screen's order after those it carried, and the Backlog screen's after
 * them when `planning` (`withPlan`). */
export function heldFromView(view: BoardView, intern: (row: RowView) => RowView, planning: boolean): HeldBoard {
  const sprints = new Map<string, SectionTotal>();
  for (const sp of view.sprints) {
    if (sp.total.rows > 0) sprints.set(sp.id, sp.total);
  }
  const columns = new Map<number, SectionTotal>();
  view.columns.forEach((col, i) => {
    if (col.total.rows > 0) columns.set(i, col.total);
  });
  const regrouped = new Map<string, string>();
  const leftOut = new Set<string>();
  for (const [status, group] of Object.entries(view.regrouped ?? {}) as [string, string][]) {
    regrouped.set(status, group);
    if (group === 'left_out') leftOut.add(status);
  }
  return {
    shape: {
      id: view.id,
      name: view.name,
      project: view.project ?? '',
      plansSprints: view.plans_sprints,
      unit: view.estimate.unit,
      counts: view.counts,
      columns: view.columns,
      sprints: view.sprints,
      issueURL: view.issue_url ?? '',
    },
    lifetime: view.lifetime,
    revision: view.revision,
    groups: view.groups ?? '',
    regrouped,
    leftOut,
    totals: { revision: view.totals_revision, all: view.all, backlog: view.backlog, sprints, columns, pace: view.pace ?? null },
    screen: view.screen,
    plan: view.plan,
    sections: withUnread(
      view.sections.map((page) => ({ section: page.section, rows: (page.rows ?? []).map(intern), next: page.next ?? null })),
      planning ? [...view.screen, ...view.plan] : view.screen,
    ),
  };
}

/** `held` after each of `wanted` it lacks, held unread in that order. */
function withUnread(held: readonly HeldSection[], wanted: readonly Section[]): HeldSection[] {
  const keys = new Set(held.map((s) => sectionKey(s.section)));
  const out = held.slice();
  for (const section of wanted) {
    const k = sectionKey(section);
    if (keys.has(k)) continue;
    keys.add(k);
    out.push({ section, rows: [], next: null, unread: true });
  }
  return out;
}

/** The board holding the Backlog screen's sections (`plan`) when
 * `planning`, each it lacks unread after those it holds; and when not,
 * none but the Board screen's, so their rows go. The board itself when
 * nothing changes. */
export function withPlan(held: HeldBoard, planning: boolean): HeldBoard {
  if (planning) {
    const sections = withUnread(held.sections, held.plan);
    return sections.length === held.sections.length ? held : { ...held, sections };
  }
  const screen = new Set(held.screen.map(sectionKey));
  const sections = held.sections.filter((s) => screen.has(sectionKey(s.section)));
  return sections.length === held.sections.length ? held : { ...held, sections };
}

/** The totals the client holds and the status groups its sections were
 * read with, for a patch read: a patch under other groups is refused as
 * stale, and the client reads the board again. */
export function heldTotalsAt(held: HeldBoard): TotalsAt {
  return { lifetime: held.lifetime, revision: held.totals.revision, groups: held.groups };
}

/** The sections the client holds, in order, for a patch read. */
export function heldSections(held: HeldBoard): Section[] {
  return held.sections.map((s) => s.section);
}

function totalsOf(t: BoardTotals): HeldTotals {
  return {
    revision: t.revision,
    all: t.all,
    backlog: t.backlog,
    sprints: new Map((t.sprints ?? []).map((s) => [s.sprint, s.total])),
    columns: new Map((t.columns ?? []).map((c) => [c.column, c.total])),
    pace: t.pace ?? null,
  };
}

// Order keys and issue IDs compare as text in code-unit order. Every key
// the providers write is ASCII (Jira's rank, Linear's encoded sort order),
// where that is the bytewise order the store sorts by.
function compareText(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

/** Rows in board order: by order key, then issue ID. */
function compareRows(a: RowView, b: RowView): number {
  return compareText(a.order, b.order) || compareText(a.card.id, b.card.id);
}

/** Whether a row's place is after the cursor. */
function afterCursor(row: RowView, cursor: SectionCursor): boolean {
  if (row.order !== cursor.order) return row.order > cursor.order;
  return row.card.id > cursor.issue;
}

/** The first index whose row sorts after `row`'s place. */
function insertionIndex(rows: readonly RowView[], row: RowView): number {
  let lo = 0;
  let hi = rows.length;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (compareRows(rows[mid], row) < 0) lo = mid + 1;
    else hi = mid;
  }
  return lo;
}

/**
 * The board after the event naming `ids` and the patch read for them,
 * every held section asked in order:
 *
 *  1. remove each ID from every section held;
 *  2. insert each row the patch returns for a section at its (order,
 *     issue) place, when that place is not after the section's cursor, or
 *     the section is loaded whole;
 *  3. take the patch's totals when it has them;
 *  4. carry the patch's lifetime and revision into every cursor.
 *
 * A section neither step changes keeps its rows array, and every row the
 * patch does not name keeps its object, so nothing drawn from them redraws.
 */
export function applyBoardPatch(held: HeldBoard, ids: readonly string[], patch: BoardPatch, intern: (row: RowView) => RowView): HeldBoard {
  if (patch.sections.length !== held.sections.length) {
    throw new Error(`tracker board: a patch answered ${patch.sections.length} sections for the ${held.sections.length} asked`);
  }
  const named = new Set(ids);
  const sections = held.sections.map((sec, i): HeldSection => {
    // A section not read holds nothing a patch could change; its first
    // page reads the board as it is then.
    if (sec.unread) return sec;
    const kept = sec.rows.some((r) => named.has(r.card.id)) ? sec.rows.filter((r) => !named.has(r.card.id)) : null;
    let rows: RowView[] | null = kept;
    for (const incoming of patch.sections[i].rows ?? []) {
      if (sec.next && afterCursor(incoming, sec.next)) continue;
      rows ??= sec.rows.slice();
      rows.splice(insertionIndex(rows, incoming), 0, intern(incoming));
    }
    const next = sec.next ? { ...sec.next, lifetime: patch.lifetime, revision: patch.revision } : null;
    return { section: sec.section, rows: rows ?? sec.rows, next };
  });
  return {
    ...held,
    lifetime: patch.lifetime,
    revision: patch.revision,
    totals: patch.totals ? totalsOf(patch.totals) : held.totals,
    sections,
  };
}

/** The board after a further page of the section at `index`: its first,
 * for a section not read. */
export function appendPage(held: HeldBoard, index: number, page: SectionPage, intern: (row: RowView) => RowView): HeldBoard {
  const sections = held.sections.slice();
  const sec = sections[index];
  sections[index] = { section: sec.section, rows: [...sec.rows, ...(page.rows ?? []).map(intern)], next: page.next ?? null };
  return { ...held, sections };
}

/** One text per section, for keys and comparison. */
export function sectionKey(s: Section): string {
  return `${s.kind}:${s.sprint ?? ''}:${s.column ?? 0}`;
}

/** A list a ticket is carried in, ranked in or moved into: a Board column
 * (`column:<i>`), or a Backlog section (`sprint:<id>`, `backlog`). */
export function columnList(column: number): string {
  return `column:${column}`;
}

/** The list a Backlog section is. */
export function sectionList(s: Section): string {
  return s.kind === 'sprint' ? `sprint:${s.sprint ?? ''}` : s.kind === 'backlog' ? 'backlog' : columnList(s.column ?? 0);
}

/** The sprint a list moves a ticket into: its ID, null for the backlog,
 * undefined for a Board column, which is no sprint. */
export function listSprint(list: string): string | null | undefined {
  if (list === 'backlog') return null;
  return list.startsWith('sprint:') ? list.slice('sprint:'.length) : undefined;
}

/** A list's rows as the board holds them now, every one whatever the
 * filter, and its name as a sentence says it; null when the board holds
 * no such list. */
export function listRows(held: HeldBoard, list: string): { readonly name: string; readonly rows: readonly RowView[] } | null {
  if (list.startsWith('column:')) {
    const col = screenColumns(held)[Number(list.slice('column:'.length))];
    return col ? { name: col.name, rows: col.rows } : null;
  }
  const sec = held.sections.find((s) => sectionList(s.section) === list && s.section.kind !== 'column');
  if (!sec) return null;
  const sprint = listSprint(list);
  const name = sprint ? (held.shape.sprints.find((sp) => sp.id === sprint)?.name || 'the sprint') : 'the backlog';
  return { name, rows: sec.rows };
}

/** One column of the Board screen: its rows as held, whether more of
 * them are still to load, and its total. */
export interface ScreenColumn {
  readonly name: string;
  /** The groups its statuses' marks draw (`markGroup`), each once, in
   * its statuses' order. */
  readonly statusGroups: readonly string[];
  readonly rows: readonly RowView[];
  /** The held section whose next page holds more of this column; null
   * when every row of the column is held. */
  readonly moreFrom: number | null;
  /** The column's total: the backend's for a column section, the held
   * rows' own for a sprint's rows loaded whole, null while not known. Both
   * leave out the rows of a status the project leaves out of counts. */
  readonly total: SectionTotal | null;
  /** How many of the held rows the total counts. */
  readonly counted: number;
  /** No page of the column has been read yet. */
  readonly unread: boolean;
}

/** The group a count puts `row` in: the one the project's status groups
 * put its status in, as the backend's totals do (BoardView.Regrouped),
 * left_out included; the one rule for a row's counted group. What its
 * mark draws is `statusMarkGroup.ts#markGroup`. */
export function groupOf(held: HeldBoard, row: RowView): string {
  return held.regrouped.get(row.card.status.id) ?? row.card.status.group ?? '';
}

/** Whether a total counts the row. */
export function counts(held: HeldBoard, row: RowView): boolean {
  return groupOf(held, row) !== 'left_out';
}

function countedOf(held: HeldBoard, rows: readonly RowView[]): number {
  if (held.leftOut.size === 0) return rows.length;
  let n = 0;
  for (const r of rows) if (counts(held, r)) n += 1;
  return n;
}

/** Whether `row` counts as a ticket without an estimate: it has none and
 * is no support ticket, which has none and is not counted as without one,
 * as the server's totals count it. Every client total counts by this. */
function withoutEstimate(row: RowView): boolean {
  return (row.estimate === undefined || row.estimate === null) && !row.support;
}

function sumOf(held: HeldBoard, rows: readonly RowView[]): SectionTotal {
  let n = 0;
  let estimate = 0;
  let unestimated = 0;
  for (const r of rows) {
    if (!counts(held, r)) continue;
    n += 1;
    estimate += r.estimate ?? 0;
    if (withoutEstimate(r)) unestimated += 1;
  }
  return { rows: n, estimate, unestimated };
}

/**
 * The Board screen's columns from the held board. A column section is a
 * column as it is. A board that plans sprints holds its active sprints'
 * rows: each goes to the column that names its status, in board order, and
 * a row whose status no column names is not shown, as on Jira's board.
 * A column built from sprint rows reuses its rows array while the held
 * sprint sections keep theirs (`previous`).
 */
export function screenColumns(held: HeldBoard, previous: readonly ScreenColumn[] = []): ScreenColumn[] {
  const { columns } = held.shape;
  const groups = (col: ColumnView) => [...new Set(col.statuses.map((s) => markGroup(held.regrouped, s) ?? ''))];
  if (!held.shape.plansSprints) {
    return columns.map((col, i): ScreenColumn => {
      const at = held.sections.findIndex((s) => s.section.kind === 'column' && (s.section.column ?? 0) === i);
      const sec = at >= 0 ? held.sections[at] : null;
      return {
        name: col.name,
        statusGroups: groups(col),
        rows: sec?.rows ?? [],
        moreFrom: sec?.next || sec?.unread ? at : null,
        total: totalOf(held.totals.columns, i),
        counted: countedOf(held, sec?.rows ?? []),
        unread: sec?.unread === true,
      };
    });
  }
  const columnOf = new Map<string, number>();
  columns.forEach((col, i) => {
    for (const st of col.statuses) columnOf.set(st.id, i);
  });
  // The Board screen's sprints: the Backlog screen's future sprints, held
  // beside them while a pane shows Backlog, are not on it.
  const onScreen = new Set(held.screen.map(sectionKey));
  const isScreenSprint = (s: HeldSection) => s.section.kind === 'sprint' && onScreen.has(sectionKey(s.section));
  const sprintSections = held.sections.filter(isScreenSprint);
  const unfinished = held.sections.findIndex((s) => isScreenSprint(s) && (s.next !== null || s.unread === true));
  const unread = sprintSections.length > 0 && sprintSections.every((s) => s.unread === true);
  const buckets: RowView[][] = columns.map(() => []);
  for (const sec of sprintSections) {
    for (const row of sec.rows) {
      const at = columnOf.get(row.card.status.id);
      if (at !== undefined) buckets[at].push(row);
    }
  }
  // Several active sprints merge in board order, as Jira shows them.
  if (sprintSections.length > 1) for (const b of buckets) b.sort(compareRows);
  return columns.map((col, i): ScreenColumn => {
    const prior = previous[i];
    const rows = prior && sameRows(prior.rows, buckets[i]) ? prior.rows : buckets[i];
    return {
      name: col.name,
      statusGroups: groups(col),
      rows,
      moreFrom: unfinished >= 0 ? unfinished : null,
      total: unfinished >= 0 ? null : sumOf(held, rows),
      counted: countedOf(held, rows),
      unread,
    };
  });
}

function sameRows(a: readonly RowView[], b: readonly RowView[]): boolean {
  return a.length === b.length && a.every((r, i) => r === b[i]);
}

/** What filters the shown cards. */
export interface CardFilter {
  /** The account whose cards Only mine keeps; null when Only mine is off. */
  readonly mine: string | null;
  /** The search's words, lower case; empty when no search is on. */
  readonly words: readonly string[];
}

/** The words of a search query, lower case. */
export function searchWords(query: string): string[] {
  return query.toLowerCase().split(/\s+/).filter((w) => w !== '');
}

/** Whether a filter is on. */
export function filtering(filter: CardFilter): boolean {
  return filter.mine !== null || filter.words.length > 0;
}

/** The text a search matches a card by: key, title, labels, parent and
 * assignee. */
function searchText(card: CardView): string {
  const parts = [card.key, card.title, card.parent?.key ?? '', card.parent?.title ?? '', card.assignee?.name ?? ''];
  for (const l of card.labels ?? []) parts.push(l.name ?? l.id);
  return parts.join('\n').toLowerCase();
}

/** Whether a card passes the filter: assigned to `mine` when set, and
 * every word found in its text. */
export function cardMatches(card: CardView, filter: CardFilter): boolean {
  if (filter.mine !== null && card.assignee?.id !== filter.mine) return false;
  if (filter.words.length === 0) return true;
  const text = searchText(card);
  return filter.words.every((w) => text.includes(w));
}

/** A column's cards as the Board screen shows them: those that pass the
 * filter, and how many of them a total counts (`counts`). */
export interface ShownColumn {
  readonly rows: readonly RowView[];
  readonly counted: number;
}

export function shownColumn(held: HeldBoard, col: ScreenColumn, filter: CardFilter): ShownColumn {
  if (!filtering(filter)) return { rows: col.rows, counted: col.counted };
  const rows = col.rows.filter((r) => cardMatches(r.card, filter));
  return { rows, counted: countedOf(held, rows) };
}

/**
 * Shares the parts of rows that many rows repeat (a type, a status, a
 * person, a priority, a label, a parent) by their content, so a board of
 * 2,000 cards holds each once. Keyed by content, not ID: a parent renamed
 * since is a new object, and the rows that still show the old name keep
 * theirs until their own patch replaces them. One interner lives as long as
 * one full read of the board.
 */
export function createRowInterner(): (row: RowView) => RowView {
  const pool = new Map<string, unknown>();
  function share<T extends object>(kind: string, value: T | null | undefined): T | null | undefined {
    if (!value) return value;
    const key = kind + JSON.stringify(value);
    const held = pool.get(key) as T | undefined;
    if (held) return held;
    pool.set(key, value);
    return value;
  }
  return (row) => {
    const card = row.card;
    card.type = share('t', card.type);
    card.status = share('s', card.status) ?? card.status;
    card.assignee = share('u', card.assignee);
    card.priority = share('p', card.priority);
    card.parent = share('e', card.parent);
    if (card.labels) card.labels = card.labels.map((l) => share('l', l) ?? l);
    return row;
  };
}

/** A card's address in the tracker: its own, else its board's with its
 * key; empty when neither is known. The one rule every surface asks. */
export function cardURL(shape: BoardShape, card: CardView): string {
  if (card.url) return card.url;
  return shape.issueURL ? shape.issueURL + encodeURIComponent(card.key) : '';
}

/** The moves of a ticket that reach each column, by column index: a move
 * reaches a column whose statuses hold the status it leads to. A column
 * no move reaches is absent. */
export function columnsReached(columns: readonly ColumnView[], moves: readonly MoveView[]): Map<number, MoveView[]> {
  const reached = new Map<number, MoveView[]>();
  columns.forEach((col, i) => {
    const into = moves.filter((m) => col.statuses.some((st) => st.id === m.to.id));
    if (into.length > 0) reached.set(i, into);
  });
  return reached;
}

/** A card drawn somewhere else while its write is out: the card of
 * `issue`, in `list`, before the card `before` names (null: at the end),
 * or where its order places it when `before` is undefined; `status` is the
 * status its move leads to. */
export interface WaitingCard {
  readonly issue: string;
  readonly list: string;
  readonly before?: string | null;
  readonly status?: string;
}

/** The row of `issue` among the lists, and the index of its list; null
 * when none holds it. */
export function rowOf(lists: readonly { readonly rows: readonly RowView[] }[], issue: string): { row: RowView; column: number } | null {
  for (let i = 0; i < lists.length; i += 1) {
    const row = lists[i].rows.find((r) => r.card.id === issue);
    if (row) return { row, column: i };
  }
  return null;
}

/**
 * The shown lists (named `keys`, held as `lists`) with each waiting card
 * drawn where it waits: its row as the board holds it now, taken out of
 * the list it shows in and put in its own, before the card `before` names
 * or where its order places it, unless it fails `filter`. A card the board
 * no longer holds, or waiting in a list not shown, is not drawn. The rows
 * only: the headers count a moved card by `columnWith`. Nothing the board
 * holds changes, and a list no waiting card touches keeps its object.
 */
export function withWaiting(
  keys: readonly string[], lists: readonly { readonly rows: readonly RowView[] }[], shown: readonly ShownColumn[], waiting: Iterable<WaitingCard>, filter: CardFilter,
): readonly ShownColumn[] {
  let out: ShownColumn[] | null = null;
  for (const w of waiting) {
    const into = keys.indexOf(w.list);
    if (into < 0) continue;
    const at = rowOf(lists, w.issue);
    if (!at) continue;
    out ??= shown.slice();
    for (let i = 0; i < out.length; i += 1) {
      if (out[i].rows.some((r) => r.card.id === w.issue)) out[i] = { rows: out[i].rows.filter((r) => r.card.id !== w.issue), counted: out[i].counted };
    }
    if (!cardMatches(at.row.card, filter)) continue;
    const rows = out[into].rows.slice();
    const k = w.before === undefined ? insertionIndex(rows, at.row) : w.before === null ? -1 : rows.findIndex((r) => r.card.id === w.before);
    rows.splice(k < 0 ? rows.length : k, 0, at.row);
    out[into] = { rows, counted: out[into].counted };
  }
  return out ?? shown;
}

/** A column and its shown rows as their header counts them with `row`
 * added (1) or taken out (-1): what the column becomes while a card is
 * carried over it or waits in it. `statuses` are those the row has in
 * this column: its own when it is taken out, the ones its move may lead
 * to when it is added; it counts only when every one of them does.
 * `inShown` is whether the row passes the filter, so the shown count
 * changes too. */
export function columnWith(
  held: HeldBoard, col: ScreenColumn, shown: ShownColumn, row: RowView, sign: 1 | -1, statuses: readonly string[], inShown: boolean,
): { col: ScreenColumn; shown: ShownColumn } {
  if (statuses.some((id) => held.leftOut.has(id))) return { col, shown };
  const total = col.total && withRow(col.total, row, sign);
  return { col: { ...col, total, counted: col.counted + sign }, shown: inShown ? { rows: shown.rows, counted: shown.counted + sign } : shown };
}

/** A total with `row` added (1) or taken out (-1): the one rule for what a
 * header counts while a ticket is carried over it or waits in it. */
export function withRow(total: SectionTotal, row: RowView, sign: 1 | -1): SectionTotal {
  return {
    rows: total.rows + sign,
    estimate: total.estimate + sign * (row.estimate ?? 0),
    unestimated: (total.unestimated ?? 0) + (withoutEstimate(row) ? sign : 0),
  };
}
