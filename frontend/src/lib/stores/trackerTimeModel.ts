// The Time screen's rules that hold no state (docs/specs/issues.md,
// "Time"): the week's days, which of them get a total, and a week's
// worklogs and agent's draft by day and by ticket. A day is `YYYY-MM-DD`
// as the tracker names it, in the author's zone; the week is Monday to
// Sunday, and this window's local date decides which week is this week.
//
// A draft entry (trackerapp.TimeEntryView) shows by its state: drafted
// (dashed, to change, log alone or remove; with no ticket, a question),
// sent (being posted), unknown (may have been logged: never sent again,
// only removed or logged by hand), posted (its worklog; until the day's
// read holds that worklog, the entry itself, drawn as logged) and left
// out (not shown on the week).

import type { TimeEntryView, WorklogDays, WorklogView } from '../../../bindings/agent-overflow/internal/trackerapp/models';

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

/** What a day holds, as the compact strip marks it: a question waiting,
 * draft time not logged (drafted, being posted, or of unknown outcome),
 * logged time, or nothing. */
export type DayMark = 'question' | 'drafted' | 'logged' | 'none';

/** One day of the week shown. */
export interface WeekDay {
  readonly day: string;
  /** Its entries, in the computer's order (day, then worklog ID). */
  readonly entries: readonly WorklogView[];
  /** The time logged on it; null when it is not counted. */
  readonly total: number | null;
  readonly uncounted: UncountedReason | null;
  /** The draft's entries on a ticket that are not logged yet (drafted,
   * sent, unknown), by ID. A posted entry is the day's worklog, or nothing
   * once that worklog is gone. */
  readonly drafted: readonly TimeEntryView[];
  /** The draft's questions: drafted entries with no ticket. */
  readonly questions: readonly TimeEntryView[];
  /** The minutes an approval of the day posts: its drafted entries on a
   * ticket. */
  readonly draftedMinutes: number;
  readonly mark: DayMark;
}

/** Whether an entry waits on the person: drafted, on a ticket or not. */
export function isDrafted(e: TimeEntryView): boolean {
  return e.state === 'drafted';
}

/** Whether a draft entry is a question: no ticket picked yet. */
export function isQuestion(e: TimeEntryView): boolean {
  return !e.key;
}

/** The entries an approval of `entries` sends: the drafted ones, the
 * questions included, which the computer leaves open and says so. An
 * entry being posted, of unknown outcome, posted or left out is never
 * sent. */
export function approvable(entries: readonly TimeEntryView[]): readonly TimeEntryView[] {
  return entries.filter(isDrafted);
}

/** The minutes an approval of `entries` posts. */
export function postedMinutes(entries: readonly TimeEntryView[]): number {
  return entries.reduce((s, e) => s + (isDrafted(e) && !isQuestion(e) ? e.minutes : 0), 0);
}

/** The week's draft as the banner counts it: the days that hold drafted
 * entries, the entries on a ticket, the questions, the minutes an
 * approval of the week posts, the threads that drafted them, and what of
 * the draft is logged: its posted entries whose worklog the read holds. */
export interface WeekDraft {
  readonly days: number;
  readonly entries: number;
  readonly questions: number;
  readonly minutes: number;
  readonly threads: readonly string[];
  readonly logged: { readonly minutes: number; readonly worklogs: number };
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
  /** Its draft entries of each day shown (`WeekDay.drafted`). */
  readonly drafted: readonly (readonly TimeEntryView[])[];
  /** The time logged on it on the counted days. */
  readonly total: number;
}

/** A week's worklogs and draft by day, as both layouts read them. */
export interface WeekShape {
  /** Monday to Friday, then Saturday and Sunday when either holds time,
   * logged or drafted. */
  readonly days: readonly WeekDay[];
  /** The time logged on the counted days. */
  readonly total: number;
  /** The days shown that have no total. */
  readonly uncounted: number;
  readonly draft: WeekDraft;
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
  // The worklogs say what is logged: a posted entry shows as its worklog,
  // and as nothing once that worklog was deleted or moved. A post whose
  // worklogs could not be read back is a receipt on the week.
  const logged = new Set(logs.map((w) => w.id));
  const draftLogged = { minutes: 0, worklogs: 0 };
  const draftByDay = new Map<string, TimeEntryView[]>();
  for (const e of answer.drafted ?? []) {
    if (e.state === 'posted' && e.worklog && logged.has(e.worklog)) {
      draftLogged.minutes += e.minutes;
      draftLogged.worklogs += 1;
    }
    if (e.state === 'left_out' || e.state === 'posted') continue;
    const list = draftByDay.get(e.day);
    if (list) list.push(e);
    else draftByDay.set(e.day, [e]);
  }
  const shownDays = all.filter((d) => !isWeekend(d) || byDay.has(d) || draftByDay.has(d));
  const threads = new Set<string>();
  const days: WeekDay[] = shownDays.map((day) => {
    const entries = byDay.get(day) ?? [];
    const uncounted: UncountedReason | null = unread.has(day) ? 'unread' : cut !== null && day >= cut ? 'partial' : null;
    const draft = draftByDay.get(day) ?? [];
    const questions = draft.filter((e) => isQuestion(e) && isDrafted(e));
    const drafted = draft.filter((e) => !isQuestion(e));
    for (const e of draft) if (isDrafted(e) && e.thread) threads.add(e.thread);
    const draftedMinutes = postedMinutes(drafted);
    // An entry being posted or of unknown outcome is not logged as far as
    // this computer knows, and waits on the person as a drafted one does.
    const mark: DayMark = questions.length > 0 ? 'question'
      : drafted.length > 0 ? 'drafted'
        : entries.length > 0 ? 'logged' : 'none';
    return { day, entries, uncounted, total: uncounted ? null : entries.reduce((s, w) => s + w.spent_seconds, 0), drafted, questions, draftedMinutes, mark };
  });
  const counted = days.filter((d) => d.total !== null).length;
  return {
    days,
    total: days.reduce((s, d) => s + (d.total ?? 0), 0),
    uncounted: days.length - counted,
    draft: {
      days: days.filter((d) => d.questions.length > 0 || d.drafted.some(isDrafted)).length,
      entries: days.reduce((s, d) => s + d.drafted.filter(isDrafted).length, 0),
      questions: days.reduce((s, d) => s + d.questions.length, 0),
      minutes: days.reduce((s, d) => s + d.draftedMinutes, 0),
      threads: [...threads].sort(),
      logged: draftLogged,
    },
  };
}

/** Every entry an approval or a discard of the week acts on: the drafted
 * ones of every day shown. */
export function weekDrafted(shape: WeekShape): readonly TimeEntryView[] {
  return shape.days.flatMap((d) => approvable([...d.drafted, ...d.questions]));
}

/** The questions of each day shown, in the order of `WeekShape.days`: By
 * ticket's last row, Not placed. */
export function weekQuestions(shape: WeekShape): readonly (readonly TimeEntryView[])[] {
  return shape.days.map((d) => d.questions);
}

/** Every ticket the week holds time on, by key: By ticket's rows and the
 * tickets the log form offers first. */
export function weekTickets(shape: WeekShape): readonly WeekTicket[] {
  const { days } = shape;
  const tickets = new Map<string, { issue: string; key: string; title: string; cells: WorklogView[][]; drafted: TimeEntryView[][]; total: number }>();
  const row = (issue: string) => {
    let t = tickets.get(issue);
    if (!t) {
      t = { issue, key: '', title: '', cells: days.map(() => []), drafted: days.map(() => []), total: 0 };
      tickets.set(issue, t);
    }
    return t;
  };
  days.forEach((d, i) => {
    for (const e of d.drafted) {
      const t = row(e.issue ?? '');
      if (e.key && !t.key) {
        t.key = e.key;
        t.title = e.title ?? '';
      }
      t.drafted[i].push(e);
    }
    for (const w of d.entries) {
      const t = row(w.issue);
      // Days read at different times disagree only after a rename, until
      // the week is refreshed; the first entry that names it wins.
      if (w.key && !t.key) {
        t.key = w.key;
        t.title = w.title ?? '';
      }
      t.cells[i].push(w);
      if (d.total !== null) t.total += w.spent_seconds;
    }
  });
  return [...tickets.values()].sort(byKey);
}

/** Where the week would be while its computer is not connected. */
export function timeOfflineText(computer: string): string {
  return `${computer} is not connected. Your time shows when it connects.`;
}

/** What a day without a total says in its place. */
export function uncountedText(reason: UncountedReason): string {
  return reason === 'unread' ? 'Not read yet' : 'Not all shown';
}

/** A ticket time is logged on, as a read named it. */
export interface WorklogTicket {
  readonly issue: string;
  /** Empty for an issue the tracker did not name. */
  readonly key: string;
  readonly title: string;
}

/** "Mon Nov 23" for a draft's day, as the computer's sentences name it. */
export function draftDayLabel(day: string): string {
  return `${DAY_NAME.format(utcOf(day))} ${MONTH_DAY.format(utcOf(day))}`;
}

/** A call's entries as its timeline card shows them: one group per day
 * the call's row names, by day, each with the entries there now; a day
 * may have none. */
export interface CardDay {
  readonly day: string;
  readonly entries: readonly TimeEntryView[];
}

export function cardDays(entries: readonly TimeEntryView[], rowDays: readonly string[]): readonly CardDay[] {
  const byDay = new Map<string, TimeEntryView[]>();
  for (const e of entries) {
    const list = byDay.get(e.day);
    if (list) list.push(e);
    else byDay.set(e.day, [e]);
  }
  const days = [...new Set([...rowDays, ...byDay.keys()])].sort();
  return days.map((day) => ({ day, entries: byDay.get(day) ?? [] }));
}

/** What a call's card says in its header, from its entries now: time to
 * approve, only questions left, time logged (some may wait in another
 * state), nothing left to approve, or no entries. */
export type CardState =
  | { readonly kind: 'draft'; readonly days: number; readonly minutes: number }
  | { readonly kind: 'questions'; readonly questions: number }
  | { readonly kind: 'logged'; readonly minutes: number; readonly worklogs: number }
  | { readonly kind: 'settled' }
  | { readonly kind: 'empty' };

export function cardState(entries: readonly TimeEntryView[]): CardState {
  const open = entries.filter(isDrafted);
  const tickets = open.filter((e) => !isQuestion(e));
  if (tickets.length > 0) return { kind: 'draft', days: new Set(open.map((e) => e.day)).size, minutes: postedMinutes(tickets) };
  if (open.length > 0) return { kind: 'questions', questions: open.length };
  const posted = entries.filter((e) => e.state === 'posted');
  if (posted.length > 0) return { kind: 'logged', minutes: posted.reduce((s, e) => s + e.minutes, 0), worklogs: posted.length };
  return entries.length > 0 ? { kind: 'settled' } : { kind: 'empty' };
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
