// A dashboard number's amount and its unit's word, as the Dashboards
// screen and a dashboard_widget card in a thread both say it. Kept apart
// from the screen's model so a thread, which loads at startup, reads the
// same rule without the Issues stores (lib/architecture.test.ts).

import { formatEstimate } from '../components/issues/boardFormat';

/** A metric's unit, as trackerdash.Unit. */
export type DashboardUnit = 'tickets' | 'estimate' | 'days' | 'time';

function trim(n: number): string {
  const rounded = Math.round(n * 10) / 10;
  return Number.isInteger(rounded) ? String(rounded) : rounded.toFixed(1);
}

/** Whether values of the unit are time in seconds (a time board's
 * estimates, logged time), which reads as hours or minutes. */
export function inTime(unit: DashboardUnit, estimates: string): boolean {
  return unit === 'time' || (unit === 'estimate' && estimates === 'time');
}

/** A value in its unit, without the unit's word: points and days to one
 * place, time as hours or minutes (inTime), tickets whole unless
 * averaged. */
export function formatAmount(v: number, unit: DashboardUnit, estimates: string): string {
  if (inTime(unit, estimates)) return formatEstimate(v, 'time') ?? '0m';
  return trim(v);
}

/** The unit's word after an amount: "3 points", "1 ticket", "2.5 days";
 * empty for time, whose amount carries it. */
export function unitWord(v: number, unit: DashboardUnit, estimates: string): string {
  const counting = unit === 'tickets' || (unit === 'estimate' && estimates === 'none');
  if (counting) return v === 1 ? 'ticket' : 'tickets';
  if (unit === 'days') return v === 1 ? 'day' : 'days';
  if (unit === 'time' || estimates === 'time') return '';
  return v === 1 ? 'point' : 'points';
}
