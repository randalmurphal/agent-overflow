// How the Board screen words a card's values. Marks are picked by the
// value's name or level: the board answer carries no tracker icon, and the
// app fetches none.

import type { ChoiceView, IssueTypeView, SectionTotal } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import type { ScreenColumn, ShownColumn } from '../../stores/trackerBoardModel';

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
