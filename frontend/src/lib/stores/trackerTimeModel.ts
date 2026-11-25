// The Time screen's rules that hold no state (docs/specs/issues.md,
// "Time"): the week's days, which of them get a total, and a week's
// worklogs by day and by ticket. A day is `YYYY-MM-DD` as the tracker
// names it, in the author's zone; the week is Monday to Sunday, and
// this window's local date decides which week is this week.

import type { WorklogDays, WorklogView } from '../../../bindings/agent-overflow/internal/trackerapp/models';

const DAY_MS = 86_400_000;
const DAY_SHAPE = /^(\d{4})-(\d{2})-(\d{2})$/;

function utcOf(day: string): number {
  const m = DAY_SHAPE.exec(day);
  if (!m) throw new Error(`trackerTimeModel: ${JSON.stringify(day)} is not a day (YYYY-MM-DD)`);
  return Date.UTC(Number(m[1]), Number(m[2]) - 1, Number(m[3]));
}

function dayOfUtc(ms: number): string {
  return new Date(ms).toISOString().slice(0, 10);
}

/** `day` moved by `n` days. */
export function addDays(day: string, n: number): string {
  return dayOfUtc(utcOf(day) + n * DAY_MS);
}

/** The Monday of `day`'s week. */
export function mondayOf(day: string): string {
  const ms = utcOf(day);
  // getUTCDay: Sunday 0 .. Saturday 6; Monday starts the week.
  const back = (new Date(ms).getUTCDay() + 6) % 7;
  return dayOfUtc(ms - back * DAY_MS);
}

/** The seven days of the week that starts on `monday`. */
export function weekDays(monday: string): readonly string[] {
  const start = utcOf(monday);
  return Array.from({ length: 7 }, (_, i) => dayOfUtc(start + i * DAY_MS));
}

/** This window's local date as a day. */
export function localDay(now: Date): string {
  const p = (n: number) => String(n).padStart(2, '0');
  return `${now.getFullYear()}-${p(now.getMonth() + 1)}-${p(now.getDate())}`;
}

/** Saturday and Sunday: the last two days of a week. */
export function isWeekend(day: string): boolean {
  const d = new Date(utcOf(day)).getUTCDay();
  return d === 0 || d === 6;
}

const DAY_NAME = new Intl.DateTimeFormat(undefined, { weekday: 'short', timeZone: 'UTC' });
const DAY_DATE = new Intl.DateTimeFormat(undefined, { day: 'numeric', timeZone: 'UTC' });
const MONTH_DAY = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', timeZone: 'UTC' });
const FULL_DATE = new Intl.DateTimeFormat(undefined, { weekday: 'short', month: 'short', day: 'numeric', year: 'numeric', timeZone: 'UTC' });

/** "Mon" and "9" for a day's header. */
export function dayHeader(day: string): { readonly name: string; readonly date: string } {
  const ms = utcOf(day);
  return { name: DAY_NAME.format(ms), date: DAY_DATE.format(ms) };
}

/** "Nov 9" for a day in a sentence. */
export function dayInSentence(day: string): string {
  return MONTH_DAY.format(utcOf(day));
}

/** "Mon, Nov 9, 2026" for a day named alone, as a form's day field. */
export function dayInFull(day: string): string {
  return FULL_DATE.format(utcOf(day));
}

/** The week's dates, "Nov 9 – 15", as the locale words a range. */
export function weekLabel(monday: string): string {
  return MONTH_DAY.formatRange(utcOf(monday), utcOf(addDays(monday, 6)));
}

/** Why a day has no total: never read for this person, or the answer
 * stopped before its end (`more`). */
export type UncountedReason = 'unread' | 'partial';

/** One day of the week shown. */
export interface WeekDay {
  readonly day: string;
  /** Its entries, in the computer's order (day, then worklog ID). */
  readonly entries: readonly WorklogView[];
  /** The time logged on it; null when it is not counted. */
  readonly total: number | null;
  readonly uncounted: UncountedReason | null;
}

/** One ticket's row of the grid. */
export interface WeekTicket {
  readonly issue: string;
  /** The key and title as the read found them; empty for an issue the
   * tracker did not return. */
  readonly key: string;
  readonly title: string;
  /** Its entries of each day shown, in the order of `WeekShape.days`. */
  readonly cells: readonly (readonly WorklogView[])[];
  /** The time logged on it on the counted days. */
  readonly total: number;
}

/** A week's worklogs as the screen lays them out. */
export interface WeekShape {
  /** Monday to Friday, then Saturday and Sunday when either holds time. */
  readonly days: readonly WeekDay[];
  /** Every ticket the week holds time on, by key. */
  readonly tickets: readonly WeekTicket[];
  /** The time logged on the counted days. */
  readonly total: number;
  /** The days shown that have no total. */
  readonly uncounted: number;
}

// "WEB-12" before "WEB-101": by project, then number; keyless tickets
// last, by issue ID.
const KEY_ORDER = new Intl.Collator(undefined, { numeric: true });

function byKey(a: WeekTicket, b: WeekTicket): number {
  if (!a.key !== !b.key) return a.key ? -1 : 1;
  return KEY_ORDER.compare(a.key || a.issue, b.key || b.issue);
}

/** The week that starts on `monday` from an answer for its seven days. */
export function weekShape(monday: string, answer: WorklogDays): WeekShape {
  const all = weekDays(monday);
  const unread = new Set(answer.unread ?? []);
  const logs = answer.worklogs ?? [];
  // With `more`, only the days before the last worklog's day are whole.
  const cut = answer.more && logs.length > 0 ? logs[logs.length - 1].day : null;
  const byDay = new Map<string, WorklogView[]>();
  for (const w of logs) {
    const list = byDay.get(w.day);
    if (list) list.push(w);
    else byDay.set(w.day, [w]);
  }
  const shownDays = all.filter((d) => !isWeekend(d) || byDay.has(d));
  const days: WeekDay[] = shownDays.map((day) => {
    const entries = byDay.get(day) ?? [];
    const uncounted: UncountedReason | null = unread.has(day) ? 'unread' : cut !== null && day >= cut ? 'partial' : null;
    return { day, entries, uncounted, total: uncounted ? null : entries.reduce((s, w) => s + w.spent_seconds, 0) };
  });
  const counted = new Set(days.filter((d) => d.total !== null).map((d) => d.day));
  const tickets = new Map<string, { issue: string; key: string; title: string; cells: WorklogView[][]; total: number }>();
  days.forEach((d, i) => {
    for (const w of d.entries) {
      let t = tickets.get(w.issue);
      if (!t) {
        t = { issue: w.issue, key: '', title: '', cells: days.map(() => []), total: 0 };
        tickets.set(w.issue, t);
      }
      // Days read at different times disagree only after a rename, until
      // the week is refreshed; the first entry that names it wins.
      if (w.key && !t.key) {
        t.key = w.key;
        t.title = w.title ?? '';
      }
      t.cells[i].push(w);
      if (counted.has(d.day)) t.total += w.spent_seconds;
    }
  });
  return {
    days,
    tickets: [...tickets.values()].sort(byKey),
    total: days.reduce((s, d) => s + (d.total ?? 0), 0),
    uncounted: days.length - counted.size,
  };
}

/** What a day without a total says in its place. */
export function uncountedText(reason: UncountedReason): string {
  return reason === 'unread' ? 'Not read yet' : 'Not all shown';
}

/** A ticket as an entry names it: its key, or the issue ID Jira did not
 * name. */
export function ticketLabel(w: { readonly issue: string; readonly key?: string }): string {
  return w.key || `ID ${w.issue}`;
}

/** The sentence for a ticket the tracker did not return. */
export function unnamedTicketText(issue: string): string {
  return `A ticket Jira does not show (ID ${issue})`;
}
