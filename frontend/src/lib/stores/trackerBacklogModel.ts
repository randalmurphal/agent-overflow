// What the Backlog screen draws of a held board (docs/specs/issues.md,
// "Backlog and sprint planning"): its sections, each header's numbers, and
// the one flat list its virtualizer shows. Pure; the board store
// (trackerBoard.svelte.ts) holds the board, and which sections Backlog
// shows is the board's computer's (`BoardView.plan`).
//
// A section's numbers: an active sprint's points done of its points; a
// future sprint's points against the board's pace (the average of its last
// finished sprints, `BoardView.pace`), each person's load and how far it
// is over; the backlog's tickets, points and tickets with no estimate. A
// board without estimates counts tickets where it would add points. A
// sprint is read whole as soon as it is held, so what its rows add up to
// is known once it is read; until then only the board's totals are.

import type { PaceView, RowView, Section, SectionTotal, SprintView, UserView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { cardMatches, filtering, rowOf, sectionKey, sectionList, totalOf, withRow, withWaiting, type CardFilter, type HeldBoard, type WaitingCard } from './trackerBoardModel';

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

function counts(held: HeldBoard, row: RowView): boolean {
  return !held.leftOut.has(row.card.status.id);
}

/** The numbers of `sec`'s header, with `moved` counted where they go. */
export function sectionNumbers(held: HeldBoard, sec: BacklogSection, moved: readonly MovedRow[]): SectionNumbers {
  let total = sec.total;
  const rows = sec.rows.slice();
  let changed = false;
  for (const m of moved) {
    if (m.from === m.to || !counts(held, m.row)) continue;
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
  const whole = !sec.unread && !sec.more;
  const pace = sec.kind === 'future' ? held.pace : null;
  return {
    total,
    before: changed ? sec.total : null,
    done: sec.kind === 'active' && whole ? doneOf(held, rows) : null,
    load: sec.kind === 'future' && whole ? loadOf(held, rows) : null,
    pace,
    over: pace ? Math.max(0, measureOf(total, held.shape.unit) - measureOf(pace, held.shape.unit)) : 0,
  };
}

function doneOf(held: HeldBoard, rows: readonly RowView[]): SectionTotal {
  let n = 0;
  let estimate = 0;
  for (const r of rows) {
    if (!counts(held, r) || r.card.status.group !== 'done') continue;
    n += 1;
    estimate += r.estimate ?? 0;
  }
  return { rows: n, estimate };
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
