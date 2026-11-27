// The Dashboards screen's pure part: how a dashboard's numbers read (a
// value in its unit, none, a gap), the change mark of a number against
// the range before, and the words a hover and a drill say of a cell.
// The numbers themselves are the board's computer's (trackerdash), one
// evaluation per answer.

import { formatAmount, unitWord, type DashboardUnit } from '../utils/dashboardAmount';

/** One number of a dashboard (trackerdash.Num, which the bindings type
 * as any for its own JSON form): a value, none ({}), not known (no `v`
 * and `nc` tickets not counted), or a gap; `nc` beside a value is the
 * tickets the metric could not count. */
export interface Num {
  readonly v?: number;
  readonly nc?: number;
  readonly gap?: boolean;
}

/** What the screen and the drill say while the board's first read has
 * not finished (an answer's `reading`). */
export const FIRST_READ_TEXT = "This board's first read has not finished: these numbers count the tickets read so far.";

/** What a gap says, in the hover value and the drill. */
export const GAP_TEXT = 'No data';

/** A number's value; null for none or a gap. */
export function numValue(n: Num | null | undefined): number | null {
  if (!n || n.gap || n.v === undefined || n.v === null) return null;
  return n.v;
}

/** What a number of tickets not counted says: "3 not counted"; empty
 * for none. */
export function notCountedWords(nc: number | null | undefined): string {
  return nc ? `${nc} not counted` : '';
}

/** A number's words, in the parts a tile lays out: its amount and the
 * unit's word ("3", "points"), the two as one (`value`: "3 points",
 * "None", "Not known" or GAP_TEXT), and its tickets not counted. */
export interface NumWords {
  readonly amount: string;
  readonly word: string;
  readonly value: string;
  readonly notCounted: string;
}

/** The one reading of a number into words; every surface that shows a
 * number takes its words from here. */
export function numWords(n: Num | null | undefined, unit: DashboardUnit, estimates: string): NumWords {
  const words = (amount: string, word = ''): NumWords => ({
    amount,
    word,
    value: word ? `${amount} ${word}` : amount,
    notCounted: n?.gap ? '' : notCountedWords(n?.nc),
  });
  if (!n) return words('None');
  if (n.gap) return words(GAP_TEXT);
  const v = numValue(n);
  if (v === null) return words(n.nc ? 'Not known' : 'None');
  return words(formatAmount(v, unit, estimates), unitWord(v, unit, estimates));
}

/** A number as one line of words: "3 points", "3 points, 2 not counted",
 * "Not known, 2 not counted", "None" or GAP_TEXT. */
export function formatNum(n: Num | null | undefined, unit: DashboardUnit, estimates: string): string {
  const w = numWords(n, unit, estimates);
  return w.notCounted ? `${w.value}, ${w.notCounted}` : w.value;
}

/** What a drill's header says of its rows: "5 tickets", or "6 rows, 5
 * tickets" when a ticket counts in more than one row. */
export function drillCountWords(rows: number, tickets: number): string {
  const t = tickets === 1 ? '1 ticket' : `${tickets} tickets`;
  if (rows === tickets) return t;
  return `${rows === 1 ? '1 row' : `${rows} rows`}, ${t}`;
}

/** The change of a number against the range before it. */
export type ChangeMark =
  | { kind: 'change'; direction: 'up' | 'down' | 'same'; tone: 'good' | 'bad' | 'neutral'; text: string }
  | { kind: 'gap'; text: string };

/** The range before as a change mark names it: its label ("the last
 * sprint") and, when it has none to compare with, why (RangeView). */
export interface RangeBefore {
  readonly previous?: string;
  readonly previous_gap?: string;
}

/**
 * The change mark of a number: none for a widget that counts now (no
 * previous value) or one without a value; a gap with the range's
 * sentence when the range before has no data; else the difference,
 * colored by the metric's good direction, with the range before's label
 * and the tickets it did not count, which could turn the direction.
 */
export function changeMark(
  value: Num | null | undefined,
  previous: Num | null | undefined,
  good: string,
  unit: DashboardUnit,
  estimates: string,
  before: RangeBefore | null | undefined,
): ChangeMark | null {
  if (!previous) return null;
  const v = numValue(value);
  if (v === null) return null;
  if (previous.gap) return { kind: 'gap', text: before?.previous_gap ?? '' };
  const p = numValue(previous);
  if (p === null) return null;
  const then = [before?.previous ?? '', previous.nc ? `${notCountedWords(previous.nc)} then` : ''].filter(Boolean).map((part) => `, ${part}`).join('');
  const delta = v - p;
  if (Math.abs(delta) < 0.05) return { kind: 'change', direction: 'same', tone: 'neutral', text: `Same as before${then}` };
  const direction = delta > 0 ? 'up' : 'down';
  const tone = good === '' ? 'neutral' : good === direction ? 'good' : 'bad';
  const amount = formatAmount(Math.abs(delta), unit, estimates);
  const word = unitWord(Math.abs(delta), unit, estimates);
  return { kind: 'change', direction, tone, text: `${direction === 'up' ? 'Up' : 'Down'} ${amount}${word ? ` ${word}` : ''} from ${formatAmount(p, unit, estimates)}${then}` };
}

/** Which way a number widget's limit warns: below it when up is good
 * news (`good`), else above it. The one rule; the board's computer stores
 * no direction (trackerapp/board_tiles.go). */
export function limitSide(good: string): 'above' | 'below' {
  return good === 'up' ? 'below' : 'above';
}

/** Whether a number is past its widget's limit on the side `limitSide`
 * names; false with no limit or no value. */
export function overLimit(value: Num | null | undefined, limit: number | null | undefined, good: string): boolean {
  const v = numValue(value);
  if (v === null || limit === null || limit === undefined) return false;
  return limitSide(good) === 'below' ? v < limit : v > limit;
}

/** A limit as words: "3 tickets", "2h". */
export function limitWords(limit: number, unit: DashboardUnit, estimates: string): string {
  const word = unitWord(limit, unit, estimates);
  return word ? `${formatAmount(limit, unit, estimates)} ${word}` : formatAmount(limit, unit, estimates);
}

/** What a number past its limit says: "over your limit of 3 tickets",
 * or "under …" where the limit warns below. */
export function pastLimitWords(limit: number, good: string, unit: DashboardUnit, estimates: string): string {
  return `${limitSide(good) === 'below' ? 'under' : 'over'} your limit of ${limitWords(limit, unit, estimates)}`;
}

/** How far one press of a limit's − or + moves it: an hour for a board
 * whose estimates are time (seconds), else 1. */
export function limitStep(unit: DashboardUnit, estimates: string): number {
  return unit === 'estimate' && estimates === 'time' ? 3600 : 1;
}
