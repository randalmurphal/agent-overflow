// How the Board screen words a card's values. Marks are picked by the
// value's name or level: the board answer carries no tracker icon, and the
// app fetches none.

import type { ChoiceView, IssueTypeView, SectionTotal } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import type { ScreenColumn, ShownColumn } from '../../stores/trackerBoardModel';
import type { SectionNumbers } from '../../stores/trackerBacklogModel';

const DAY_MS = 86_400_000;

/** An estimate in the board's unit: points as they are, time (seconds) as
 * hours or minutes; null for a board without estimates or a row with none. */
export function formatEstimate(value: number | null | undefined, unit: string): string | null {
  if (value == null || unit === 'none' || unit === '') return null;
  if (unit === 'time') {
    if (value < 3600) return `${Math.round(value / 60)}m`;
    return `${trim(value / 3600)}h`;
  }
  return trim(value);
}

function trim(n: number): string {
  return Number.isInteger(n) ? String(n) : n.toFixed(1).replace(/\.0$/, '');
}

/** A column's count and, for a board with estimates, its sum. */
function formatTotal(total: SectionTotal, unit: string): string {
  const sum = formatEstimate(total.estimate, unit);
  return sum === null ? String(total.rows) : `${total.rows} · ${sum}${unit === 'points' ? ' pts' : ''}`;
}

/** A column's header count, every number of it a count of the rows a
 * total counts: its total, or the held rows while it has none; under a
 * filter the matching rows (`shown`), or how much of the column has loaded
 * while it has more. */
export function columnCount(col: ScreenColumn, shown: ShownColumn, filtered: boolean, unit: string): string {
  if (filtered) {
    if (col.moreFrom === null) return String(shown.counted);
    return col.total ? `${col.counted} of ${col.total.rows} loaded` : `${col.counted} loaded`;
  }
  return col.total ? formatTotal(col.total, unit) : String(col.counted);
}

/** Whole days since the card entered its status; 0 when unknown. */
export function daysInStatus(since: number | undefined, now: number): number {
  if (!since || since > now) return 0;
  return Math.floor((now - since) / DAY_MS);
}

export type TypeMark = 'epic' | 'subtask' | 'bug' | 'story' | 'task' | 'other';

export function typeMark(type: IssueTypeView | null | undefined): TypeMark {
  if (!type) return 'other';
  if (type.level >= 1) return 'epic';
  if (type.level < 0) return 'subtask';
  const name = (type.name ?? '').toLowerCase();
  if (name.includes('bug') || name.includes('defect')) return 'bug';
  if (name.includes('story')) return 'story';
  if (name.includes('task')) return 'task';
  return 'other';
}

export type PriorityMark = 'urgent' | 'highest' | 'high' | 'medium' | 'low' | 'lowest' | 'none' | 'other';

const PRIORITY_NAMES: Readonly<Record<string, PriorityMark>> = {
  urgent: 'urgent', blocker: 'urgent', critical: 'urgent',
  highest: 'highest', high: 'high', major: 'high',
  medium: 'medium', normal: 'medium',
  low: 'low', minor: 'low', lowest: 'lowest', trivial: 'lowest',
  'no priority': 'none', none: 'none',
};

export function priorityMark(priority: ChoiceView | null | undefined): PriorityMark {
  if (!priority) return 'none';
  return PRIORITY_NAMES[(priority.name ?? '').trim().toLowerCase()] ?? 'other';
}

/** A part of a Backlog section's numbers line: plain, what a moved row
 * makes it (`to`, after an arrow), or a warning. */
export interface LinePart {
  readonly text: string;
  readonly tone?: 'to' | 'warn';
}

/** A Backlog section's numbers line (`SectionNumbers`), in the board's
 * measure: its points or time, else its tickets. An active sprint says how
 * much is done once it is read; a future sprint its total of about the
 * pace, and how far over; the backlog its tickets, points and tickets with
 * no estimate. A moved row shows as the number before, an arrow and the
 * number it makes. */
export function sectionLine(kind: 'active' | 'future' | 'backlog', n: SectionNumbers, unit: string): LinePart[] {
  const estimated = unit === 'points' || unit === 'time';
  const measure = (t: { rows: number; estimate: number }) => (estimated ? t.estimate : t.rows);
  const bare = (v: number) => (estimated ? (formatEstimate(v, unit) ?? '0') : trim(v));
  const word = (v: number) => (unit === 'points' ? (v === 1 ? ' point' : ' points') : unit === 'time' ? '' : v === 1 ? ' ticket' : ' tickets');
  const tickets = (v: number) => (v === 1 ? ' ticket' : ' tickets');
  const parts: LinePart[] = [];
  /** `now`, after `was` and an arrow when a moved row changes it. */
  const moving = (was: number | null, now: number, after: string) => {
    if (was !== null && was !== now) parts.push({ text: `${bare(was)} ` }, { text: `→ ${bare(now)}${after}`, tone: 'to' });
    else parts.push({ text: `${bare(now)}${after}` });
  };
  const now = measure(n.total);
  const was = n.before ? measure(n.before) : null;
  if (kind === 'active') {
    if (n.done) parts.push({ text: `${bare(measure(n.done))} of ` });
    moving(was, now, `${word(now)}${n.done ? ' done' : ''}`);
    return parts;
  }
  if (kind === 'future') {
    if (!n.pace) {
      moving(was, now, word(now));
      return parts;
    }
    const pace = measure(n.pace);
    moving(was, now, '');
    const last = n.pace.sprints === 1 ? 'the last sprint' : `the last ${n.pace.sprints} sprints' average`;
    parts.push({ text: ` of about ${bare(Math.round(pace * 10) / 10)}${word(pace)}, ${last}` });
    if (n.over > 0) parts.push({ text: ' · ' }, { text: `${bare(Math.round(n.over * 10) / 10)} over`, tone: 'warn' });
    return parts;
  }
  const wasRows = n.before ? n.before.rows : null;
  if (wasRows !== null && wasRows !== n.total.rows) parts.push({ text: `${wasRows} ` }, { text: `→ ${n.total.rows}${tickets(n.total.rows)}`, tone: 'to' });
  else parts.push({ text: `${n.total.rows}${tickets(n.total.rows)}` });
  if (!estimated) return parts;
  parts.push({ text: ' · ' });
  moving(n.before ? n.before.estimate : null, n.total.estimate, word(n.total.estimate));
  const none = n.total.unestimated ?? 0;
  if (none > 0) parts.push({ text: ' · ' }, { text: `${none} without an estimate`, tone: 'warn' });
  return parts;
}
