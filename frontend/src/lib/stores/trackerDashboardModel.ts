// The Dashboards screen's pure part: how a dashboard's numbers read (a
// value in its unit, none, a gap), the change mark of a number against
// the range before, and the words a hover and a drill say of a cell.
// The numbers themselves are the board's computer's (trackerdash), one
// evaluation per answer.

import { formatEstimate } from '../components/issues/boardFormat';

/** One number of a dashboard (trackerdash.Num, which the bindings type
 * as any for its own JSON form): a value, none ({}), or a gap. */
export interface Num {
  readonly v?: number;
  readonly gap?: boolean;
}

/** A metric's unit, as trackerdash.Unit. */
export type DashboardUnit = 'tickets' | 'estimate' | 'days';
/** A board's estimate, as trackerdash.Answer.estimates. */
export type DashboardEstimates = 'points' | 'time' | 'none';

/** What a gap says, in the hover value and the drill. */
export const GAP_TEXT = 'No data';

/** A number's value; null for none or a gap. */
export function numValue(n: Num | null | undefined): number | null {
  if (!n || n.gap || n.v === undefined || n.v === null) return null;
  return n.v;
}

function trim(n: number): string {
  const rounded = Math.round(n * 10) / 10;
  return Number.isInteger(rounded) ? String(rounded) : rounded.toFixed(1);
}

/** A value in its unit, without the unit's word: points and days to one
 * place, time as hours or minutes, tickets whole unless averaged. */
export function formatAmount(v: number, unit: DashboardUnit, estimates: string): string {
  if (unit === 'estimate' && estimates === 'time') return formatEstimate(v, 'time') ?? '0m';
  return trim(v);
}

/** The unit's word after an amount: "3 points", "1 ticket", "2.5 days";
 * empty for time, whose amount carries it. */
export function unitWord(v: number, unit: DashboardUnit, estimates: string): string {
  const counting = unit === 'tickets' || (unit === 'estimate' && estimates === 'none');
  if (counting) return v === 1 ? 'ticket' : 'tickets';
  if (unit === 'days') return v === 1 ? 'day' : 'days';
  if (estimates === 'time') return '';
  return v === 1 ? 'point' : 'points';
}

/** A number as a person reads it: "3 points", "None", or GAP_TEXT. */
export function formatNum(n: Num | null | undefined, unit: DashboardUnit, estimates: string): string {
  if (!n) return 'None';
  if (n.gap) return GAP_TEXT;
  const v = numValue(n);
  if (v === null) return 'None';
  const word = unitWord(v, unit, estimates);
  return word ? `${formatAmount(v, unit, estimates)} ${word}` : formatAmount(v, unit, estimates);
}

/** The change of a number against the range before it. */
export type ChangeMark =
  | { kind: 'change'; direction: 'up' | 'down' | 'same'; tone: 'good' | 'bad' | 'neutral'; text: string }
  | { kind: 'gap'; text: string };

/**
 * The change mark of a number: none for a widget that counts now (no
 * previous value) or one without a value; a gap with `gapText` (the
 * range's sentence) when the range before has no data; else the
 * difference, colored by the metric's good direction.
 */
export function changeMark(
  value: Num | null | undefined,
  previous: Num | null | undefined,
  good: string,
  unit: DashboardUnit,
  estimates: string,
  gapText: string,
): ChangeMark | null {
  if (!previous) return null;
  const v = numValue(value);
  if (v === null) return null;
  if (previous.gap) return { kind: 'gap', text: gapText };
  const p = numValue(previous);
  if (p === null) return null;
  const delta = v - p;
  if (Math.abs(delta) < 0.05) return { kind: 'change', direction: 'same', tone: 'neutral', text: 'Same as before' };
  const direction = delta > 0 ? 'up' : 'down';
  const tone = good === '' ? 'neutral' : good === direction ? 'good' : 'bad';
  const amount = formatAmount(Math.abs(delta), unit, estimates);
  const word = unitWord(Math.abs(delta), unit, estimates);
  return { kind: 'change', direction, tone, text: `${direction === 'up' ? 'Up' : 'Down'} ${amount}${word ? ` ${word}` : ''} from ${formatAmount(p, unit, estimates)}` };
}

/** A range's widget is in it: "Last 5 sprints" reads "the last 5 sprints". */
export function lowerFirst(s: string): string {
  return s ? s[0].toLowerCase() + s.slice(1) : s;
}
