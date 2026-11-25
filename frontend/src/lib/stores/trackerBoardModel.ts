// What the client holds of a tracker board, and the pure rules that change
// it: the patch rule, a further page, the grouping of a sprint's rows into
// the board's columns, and the Only mine and Search match. Which sections
// the Board screen reads is the board's computer's (`BoardView.screen`). No state lives here; the board store
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
  RowView,
  Section,
  SectionCursor,
  SectionPage,
  SectionTotal,
  SprintView,
  TotalsAt,
} from '../../../bindings/agent-overflow/internal/trackerapp/models';

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

/** Every section's total at the board's totals revision. A sprint or
 * column a map lacks holds no row. */
export interface HeldTotals {
  readonly revision: number;
  readonly all: SectionTotal;
  readonly backlog: SectionTotal;
  readonly sprints: ReadonlyMap<string, SectionTotal>;
  readonly columns: ReadonlyMap<number, SectionTotal>;
}

/** The board's structure as the screen draws it. */
export interface BoardShape {
  readonly id: string;
  readonly name: string;
  readonly plansSprints: boolean;
  /** none, points, or time (seconds). */
  readonly unit: string;
  readonly columns: readonly ColumnView[];
  readonly sprints: readonly SprintView[];
}

/** What the client holds of a board: its shape, lifetime and revision,
 * the version of the project's status groups it was read with (empty for
 * none) and the statuses those groups leave out of counts, the totals, and
 * the sections it reads. */
export interface HeldBoard {
  readonly shape: BoardShape;
  readonly lifetime: number;
  readonly revision: number;
  readonly groups: string;
  readonly leftOut: ReadonlySet<string>;
  readonly totals: HeldTotals;
  readonly sections: readonly HeldSection[];
}

const NO_ROWS: SectionTotal = { rows: 0, estimate: 0 };

/** A total, none when the map lacks it. */
export function totalOf<K>(map: ReadonlyMap<K, SectionTotal>, key: K): SectionTotal {
  return map.get(key) ?? NO_ROWS;
}

/** The board a full read answered, its pages interned through `intern`,
 * with each Board screen section it carried no page of held unread, in the
 * screen's order after those it carried. */
export function heldFromView(view: BoardView, intern: (row: RowView) => RowView): HeldBoard {
  const sprints = new Map<string, SectionTotal>();
  for (const sp of view.sprints) {
    if (sp.total.rows > 0) sprints.set(sp.id, sp.total);
  }
  const columns = new Map<number, SectionTotal>();
  view.columns.forEach((col, i) => {
    if (col.total.rows > 0) columns.set(i, col.total);
  });
  return {
    shape: {
      id: view.id,
      name: view.name,
      plansSprints: view.plans_sprints,
      unit: view.estimate.unit,
      columns: view.columns,
      sprints: view.sprints,
    },
    lifetime: view.lifetime,
    revision: view.revision,
    groups: view.groups ?? '',
    leftOut: new Set(view.left_out ?? []),
    totals: { revision: view.totals_revision, all: view.all, backlog: view.backlog, sprints, columns },
    sections: [
      ...view.sections.map((page) => ({ section: page.section, rows: (page.rows ?? []).map(intern), next: page.next ?? null })),
      ...unreadScreenSections(view),
    ],
  };
}

function unreadScreenSections(view: BoardView): HeldSection[] {
  const carried = new Set(view.sections.map((page) => sectionKey(page.section)));
  return view.screen.filter((s) => !carried.has(sectionKey(s))).map((section) => ({ section, rows: [], next: null, unread: true }));
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
    shape: held.shape,
    lifetime: patch.lifetime,
    revision: patch.revision,
    groups: held.groups,
    leftOut: held.leftOut,
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

/** One column of the Board screen: its rows as held, whether more of
 * them are still to load, and its total. */
export interface ScreenColumn {
  readonly name: string;
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

/** Whether a total counts the row: not when the project leaves its status
 * out of counts, as the backend's totals do (BoardView.left_out). */
function counts(held: HeldBoard, row: RowView): boolean {
  return !held.leftOut.has(row.card.status.id);
}

function countedOf(held: HeldBoard, rows: readonly RowView[]): number {
  if (held.leftOut.size === 0) return rows.length;
  let n = 0;
  for (const r of rows) if (counts(held, r)) n += 1;
  return n;
}

function sumOf(held: HeldBoard, rows: readonly RowView[]): SectionTotal {
  let n = 0;
  let estimate = 0;
  for (const r of rows) {
    if (!counts(held, r)) continue;
    n += 1;
    estimate += r.estimate ?? 0;
  }
  return { rows: n, estimate };
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
  const groups = (col: ColumnView) => [...new Set(col.statuses.map((s) => s.group ?? ''))];
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
  const sprintSections = held.sections.filter((s) => s.section.kind === 'sprint');
  const unfinished = held.sections.findIndex((s) => s.section.kind === 'sprint' && (s.next !== null || s.unread === true));
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
