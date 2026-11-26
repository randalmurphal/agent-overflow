// What the Backlog screen draws of a held board (docs/specs/issues.md,
// "Backlog and sprint planning"): its sections, each header's numbers, and
// the one flat list its virtualizer shows. Pure; the board store
// (trackerBoard.svelte.ts) holds the board, and which sections Backlog
// shows is the board's computer's (`BoardView.plan`).
//
// A section's numbers: an active sprint's points done of its points; a
// future sprint's points against the board's pace (the average of its last
// finished sprints, held with the totals), each person's load and how far it
// is over; the backlog's tickets, points and tickets with no estimate. A
// board without estimates counts tickets where it would add points. A
// sprint is read whole as soon as it is held, so what its rows add up to
// is known once it is read; until then only the board's totals are.

import type { PaceView, RowView, Section, SectionTotal, SprintView, UserView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { cardMatches, counts, filtering, groupOf, rowOf, sectionKey, sectionList, totalOf, withRow, withWaiting, type CardFilter, type HeldBoard, type WaitingCard } from './trackerBoardModel';

export type BacklogKind = 'active' | 'future' | 'backlog';

/** A section of the Backlog screen as the board holds it. */
export interface BacklogSection {
  /** The list a carry and a write name it by (`sectionList`). */
  readonly list: string;
  readonly kind: BacklogKind;
  readonly section: Section;
  /** Its sprint; null for the backlog. */
  readonly sprint: SprintView | null;
  /** The rows held, in board order, whatever the filter. */
  readonly rows: readonly RowView[];
  /** No page of it has been read yet. */
  readonly unread: boolean;
  /** More of it is still to read. */
  readonly more: boolean;
  /** The board's total of it. */
  readonly total: SectionTotal;
}

/** The Backlog screen's sections, in the board's order (`plan`): each
 * open sprint, then the backlog. A plan section the board does not hold
 * (no pane on Backlog held it yet) is unread. */
export function backlogSections(held: HeldBoard): BacklogSection[] {
  const bySection = new Map(held.sections.map((s) => [sectionKey(s.section), s]));
  return held.plan.map((section): BacklogSection => {
    const sec = bySection.get(sectionKey(section));
    const sprint = section.kind === 'sprint' ? held.shape.sprints.find((sp) => sp.id === section.sprint) ?? null : null;
    return {
      list: sectionList(section),
      kind: section.kind === 'backlog' ? 'backlog' : sprint?.state === 'active' ? 'active' : 'future',
      section,
      sprint,
      rows: sec?.rows ?? [],
      unread: !sec || sec.unread === true,
      more: sec?.next != null,
      total: section.kind === 'backlog' ? held.totals.backlog : totalOf(held.totals.sprints, section.sprint ?? ''),
    };
  });
}

/** The sections' rows as the screen draws them: those that pass the
 * filter, with each waiting row drawn where it waits. */
export function shownSections(sections: readonly BacklogSection[], waiting: Iterable<WaitingCard>, filter: CardFilter): readonly (readonly RowView[])[] {
  const on = filtering(filter);
  const shown = sections.map((s) => ({ rows: on ? s.rows.filter((r) => cardMatches(r.card, filter)) : s.rows, counted: 0 }));
  return withWaiting(sections.map((s) => s.list), sections, shown, waiting, filter).map((s) => s.rows);
}

/** A row counted in another section than the one holding it: waiting
 * there, or carried over it. */
export interface MovedRow {
  readonly row: RowView;
  readonly from: string;
  readonly to: string;
}

/** The rows waiting in another section than the one that holds them. */
export function waitingMoves(sections: readonly BacklogSection[], waiting: Iterable<WaitingCard>): MovedRow[] {
  const out: MovedRow[] = [];
  for (const w of waiting) {
    const at = rowOf(sections, w.issue);
    if (at && sections[at.column].list !== w.list && sections.some((s) => s.list === w.list)) out.push({ row: at.row, from: sections[at.column].list, to: w.list });
  }
  return out;
}

/** One person's share of a sprint: their rows and what those add up to. */
export interface PersonLoad {
  /** Null for the rows assigned to nobody. */
  readonly person: UserView | null;
  readonly rows: number;
  readonly estimate: number;
}

/** What a section's header says. */
export interface SectionNumbers {
  /** The section's total, with the moved rows counted where they go. */
  readonly total: SectionTotal;
  /** That total before the moved rows: what an arrow leads from. Null when
   * no row moves in or out. */
  readonly before: SectionTotal | null;
  /** An active sprint's rows in a done status; null for another section,
   * or until the sprint is read. */
  readonly done: SectionTotal | null;
  /** A future sprint's load per person, the most first, nobody's last;
   * null for another section, or until the sprint is read. */
  readonly load: readonly PersonLoad[] | null;
  /** A future sprint's pace; null for another section, or a board that
   * finished no sprint. */
  readonly pace: PaceView | null;
  /** How far a future sprint is over its pace, in the board's measure
   * (`measureOf`); 0 when it is not. */
  readonly over: number;
}

/** What a board adds up: its estimates, or its tickets on a board without
 * estimates. */
export function measureOf(total: { readonly rows: number; readonly estimate: number }, unit: string): number {
  return unit === 'points' || unit === 'time' ? total.estimate : total.rows;
}

/** The numbers of `sec`'s header, counted by the project's status groups,
 * with `moved` counted where they go. */
export function sectionNumbers(held: HeldBoard, sec: BacklogSection, moved: readonly MovedRow[]): SectionNumbers {
  const whole = !sec.unread && !sec.more;
  const { total, rows, changed } = withMoved(sec, moved, sec.total, (r) => counts(held, r));
  const pace = sec.kind === 'future' ? held.totals.pace : null;
  return {
    total,
    before: changed ? sec.total : null,
    done: sec.kind === 'active' && whole ? sumWhere(rows, (r) => groupOf(held, r) === 'done') : null,
    load: sec.kind === 'future' && whole ? loadOf(held, rows) : null,
    pace,
    over: pace ? Math.max(0, measureOf(total, held.shape.unit) - measureOf(pace, held.shape.unit)) : 0,
  };
}

/** What the Complete sprint form says of `sec`, an active sprint, with
 * `moved` counted where they go: its tickets and those Jira's completion
 * counts finished (`numbers`, as its header's line reads them), and how
 * many it does not, which the completion moves (`unfinished`). Jira
 * leaves no ticket out, so a whole sprint's total is its rows'; until the
 * sprint is read whole its total is the board's and nothing else is
 * counted (null). */
export interface Completion {
  readonly numbers: SectionNumbers;
  readonly unfinished: number | null;
}

export function completionNumbers(held: HeldBoard, sec: BacklogSection, moved: readonly MovedRow[]): Completion {
  const whole = !sec.unread && !sec.more;
  const start = whole ? sumWhere(sec.rows, () => true) : sec.total;
  const { total, rows, changed } = withMoved(sec, moved, start, () => true);
  const finished = finishedStatuses(held.shape.columns);
  const done = whole ? sumWhere(rows, (r) => finished.has(r.card.status.id)) : null;
  return {
    numbers: { total, before: changed ? start : null, done, load: null, pace: null, over: 0 },
    unfinished: done ? rows.length - done.rows : null,
  };
}

/** The statuses Jira counts finished when it completes a sprint of the
 * board whose columns are `columns`: those of its last column that has
 * any, as the completion's write does (`BoardConfig.DoneStatusIDs` in
 * internal/tracker/jira). None when no column has a status, a board Jira
 * refuses to complete a sprint of. The board's columns are as this
 * computer last read them; the write reads Jira's own. */
export function finishedStatuses(columns: readonly { readonly statuses: readonly { readonly id: string }[] }[]): ReadonlySet<string> {
  for (let i = columns.length - 1; i >= 0; i -= 1) {
    if (columns[i].statuses.length > 0) return new Set(columns[i].statuses.map((st) => st.id));
  }
  return new Set();
}

/** `sec`'s total from `start` and its rows, with each of `moved` that
 * `counted` counts taken out of the section it leaves and added to the
 * one it goes to; `changed` when one moved in or out. */
function withMoved(sec: BacklogSection, moved: readonly MovedRow[], start: SectionTotal, counted: (row: RowView) => boolean): { total: SectionTotal; rows: RowView[]; changed: boolean } {
  let total = start;
  const rows = sec.rows.slice();
  let changed = false;
  for (const m of moved) {
    if (m.from === m.to || !counted(m.row)) continue;
    if (m.from === sec.list) {
      total = withRow(total, m.row, -1);
      const at = rows.findIndex((r) => r.card.id === m.row.card.id);
      if (at >= 0) rows.splice(at, 1);
      changed = true;
    } else if (m.to === sec.list) {
      total = withRow(total, m.row, 1);
      rows.push(m.row);
      changed = true;
    }
  }
  return { total, rows, changed };
}

const NO_ROWS: SectionTotal = { rows: 0, estimate: 0, unestimated: 0 };

/** The rows of `rows` that `where` keeps, added up. */
function sumWhere(rows: readonly RowView[], where: (row: RowView) => boolean): SectionTotal {
  let total = NO_ROWS;
  for (const r of rows) if (where(r)) total = withRow(total, r, 1);
  return total;
}

function loadOf(held: HeldBoard, rows: readonly RowView[]): PersonLoad[] {
  const by = new Map<string, { person: UserView | null; rows: number; estimate: number }>();
  for (const r of rows) {
    if (!counts(held, r)) continue;
    const id = r.card.assignee?.id ?? '';
    let load = by.get(id);
    if (!load) by.set(id, (load = { person: r.card.assignee ?? null, rows: 0, estimate: 0 }));
    load.rows += 1;
    load.estimate += r.estimate ?? 0;
  }
  const unit = held.shape.unit;
  return [...by.values()].sort((a, b) => (a.person === null ? 1 : b.person === null ? -1 : measureOf(b, unit) - measureOf(a, unit)));
}

/** One item of the screen's one list, at `at` in it: a section's header,
 * one of its rows (`index` among its shown rows), or its foot (where a
 * ticket is made, or the empty section's drop place). A folded section is
 * its header alone. */
export type BacklogItem =
  | { readonly kind: 'head'; readonly key: string; readonly at: number; readonly section: number }
  | { readonly kind: 'row'; readonly key: string; readonly at: number; readonly section: number; readonly index: number; readonly row: RowView }
  | { readonly kind: 'foot'; readonly key: string; readonly at: number; readonly section: number };

/** The list the screen shows: each section's header, then, when it is
 * open, its shown rows and its foot. */
export function backlogItems(sections: readonly BacklogSection[], shown: readonly (readonly RowView[])[], open: (sec: BacklogSection) => boolean): BacklogItem[] {
  const items: BacklogItem[] = [];
  sections.forEach((sec, i) => {
    items.push({ kind: 'head', key: `h:${sec.list}`, at: items.length, section: i });
    if (!open(sec)) return;
    shown[i].forEach((row, index) => items.push({ kind: 'row', key: `r:${row.card.id}`, at: items.length, section: i, index, row }));
    items.push({ kind: 'foot', key: `f:${sec.list}`, at: items.length, section: i });
  });
  return items;
}

/** Where a carried row would go with the pointer over `items[at]`, in its
 * lower half when `below`: a place among the shown rows of that item's
 * section (an index into them, the carried row's own place included). A
 * header is the top of its section, a foot its end. */
export function spotAt(items: readonly BacklogItem[], shown: readonly (readonly RowView[])[], sections: readonly BacklogSection[], at: number, below: boolean): { list: string; slot: number } | null {
  const item = items[at];
  if (!item) return null;
  const list = sections[item.section].list;
  if (item.kind === 'head') return { list, slot: 0 };
  if (item.kind === 'foot') return { list, slot: shown[item.section].length };
  return { list, slot: item.index + (below ? 1 : 0) };
}

/** The item index a slot of `list` opens its gap at: the row it goes
 * before, else the section's foot; -1 when the section shows no item for
 * it (folded). */
export function gapItem(items: readonly BacklogItem[], sections: readonly BacklogSection[], list: string, slot: number): number {
  const section = sections.findIndex((s) => s.list === list);
  if (section < 0) return -1;
  const row = items.findIndex((it) => it.section === section && it.kind === 'row' && it.index === slot);
  if (row >= 0) return row;
  return items.findIndex((it) => it.section === section && it.kind === 'foot');
}

/** The name a new sprint is offered: the last sprint's with its trailing
 * number one more ("Sprint 41" after "Sprint 40"), else empty. */
export function nextSprintName(sprints: readonly SprintView[]): string {
  for (let i = sprints.length - 1; i >= 0; i -= 1) {
    const m = /^(.*?)(\d+)$/.exec(sprints[i].name.trim());
    if (m) return `${m[1]}${Number(m[2]) + 1}`;
  }
  return '';
}

/** A day as a date input holds it (YYYY-MM-DD), in this computer's zone. */
export function dayValue(ms: number): string {
  const d = new Date(ms);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

/** The start (00:00) or the end (23:59) of a date input's day in this
 * computer's zone, in Unix ms; null for no day. */
export function dayAt(value: string, end: boolean): number | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value);
  if (!m) return null;
  return new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]), end ? 23 : 0, end ? 59 : 0).getTime();
}
