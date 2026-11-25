// The weeks of logged time the Time screen shows (docs/specs/issues.md,
// "Time"; the reads in docs/architecture/tracker-backend.md, "Methods and
// events"). A week is one person's worklogs of seven days, Monday to
// Sunday, on one connection, read from the computer that holds it.
//
// Two reads, both of the whole week. The stored read (`TrackerWorklogs`)
// answers the computer's copy and sends nothing to the tracker; the
// refresh (`RefreshTrackerWorklogs`) reads the tracker first, then answers
// the copy. Both answer a snapshot of the copy taken when the computer
// answers, and one connection delivers answers in order, so the answer
// that arrives last is the one shown. A refresh's `failure` is kept beside
// the worklogs until the next refresh answers; a stored read does not
// clear it.
//
// Nothing is read while the week is not shown (the pane hidden or covered,
// the window hidden). When it shows (attached shown, or shown again), the
// stored read runs, and so does a refresh unless this page refreshed the
// week less than five minutes ago. A refresh also runs on Refresh, when
// the computer connects again, and when a write asks for its days
// (`refreshTrackerWorklogDays`); a write that was made asks for the stored
// read of its days instead (`rereadTrackerWorklogDays`), since the write
// stored them. No timer runs: a week nobody touches is read once.
//
// The five weeks used last are held, each with its last answer, so a week
// the person returns to shows at once; a sixth drops the one used least
// recently that no surface holds.

import { SvelteMap } from 'svelte/reactivity';
import type { FailureView, WorklogDays } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { RefreshTrackerWorklogs, TrackerWorklogs } from './bindings';
import { onBackendStatusChange } from './transportStatus.svelte';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { documentHidden } from '../utils/pageVisibility';
import { addDays } from './trackerTimeModel';

/** One person's week on one connection. */
export interface TrackerWeekRef {
  readonly backend: BackendKey;
  readonly connection: string;
  /** The tracker's account ID of the person (the connection's verified
   * account). */
  readonly author: string;
  /** The week's Monday, YYYY-MM-DD. */
  readonly monday: string;
}

/** A week as the screen draws it. Replaced whole on every change. */
export interface TrackerWeekState {
  /** The last answer, of either read; null before one arrived. */
  readonly days: WorklogDays | null;
  /** Why the last refresh could not read the tracker; null when it could.
   * The worklogs are then as the computer held them before. */
  readonly failure: FailureView | null;
  /** The error of the last read the computer refused (any but a dropped
   * connection); null once a later read answers. */
  readonly readError: unknown;
  readonly refreshing: boolean;
}

/** A week this page refreshed less than this long ago is not refreshed
 * again when it shows. */
export const WEEK_REFRESH_AFTER_MS = 5 * 60_000;
/** The weeks held, the ones in use included. */
export const WEEKS_HELD = 5;

interface Week {
  readonly ref: TrackerWeekRef;
  state: TrackerWeekState;
  holders: number;
  shown: boolean;
  /** Whether the stored read and the refresh rule ran since it last
   * showed. */
  visible: boolean;
  /** When this page last sent a refresh of it (Date.now), 0 for never. */
  refreshedAt: number;
  refresh: Promise<void> | null;
}

const EMPTY: TrackerWeekState = { days: null, failure: null, readError: null, refreshing: false };

const weeks = new Map<string, Week>();
// The state each surface reads, by key: a week's reactive box.
const states = new SvelteMap<string, TrackerWeekState>();

export function trackerWeekKey(ref: TrackerWeekRef): string {
  return `${ref.backend}\n${ref.connection}\n${ref.author}\n${ref.monday}`;
}

function update(week: Week, change: Partial<TrackerWeekState>): void {
  week.state = { ...week.state, ...change };
  const key = trackerWeekKey(week.ref);
  if (weeks.get(key) === week) states.set(key, week.state);
}

function visibleOf(week: Week): boolean {
  return week.holders > 0 && week.shown && !documentHidden();
}

async function read(week: Week, refresh: boolean): Promise<void> {
  const { backend, connection, author, monday } = week.ref;
  const call = refresh ? RefreshTrackerWorklogs : TrackerWorklogs;
  try {
    const days = await withBackendTarget(backend, () => call(connection, author, monday, addDays(monday, 6)));
    update(week, refresh ? { days, failure: days.failure ?? null, readError: null } : { days, readError: null });
  } catch (err) {
    // A dropped connection says nothing new: the screen says the computer
    // is not connected, and the reconnect refreshes.
    if (isPassiveConnectionFailure(err)) return;
    update(week, { readError: err });
  }
}

function refresh(week: Week): Promise<void> {
  if (week.refresh) return week.refresh;
  week.refreshedAt = Date.now();
  update(week, { refreshing: true });
  const run = read(week, true).finally(() => {
    week.refresh = null;
    update(week, { refreshing: false });
  });
  week.refresh = run;
  return run;
}

// The week shows now or stopped showing: on showing, its stored read, and
// a refresh unless one ran in the last five minutes.
function settle(week: Week): void {
  const visible = visibleOf(week);
  if (visible === week.visible) return;
  week.visible = visible;
  if (!visible) return;
  void read(week, false);
  if (Date.now() - week.refreshedAt >= WEEK_REFRESH_AFTER_MS) void refresh(week);
}

function hold(ref: TrackerWeekRef): Week {
  const key = trackerWeekKey(ref);
  let week = weeks.get(key);
  if (week) {
    // Re-inserted: the week used last is kept longest.
    weeks.delete(key);
  } else {
    week = { ref, state: EMPTY, holders: 0, shown: false, visible: false, refreshedAt: 0, refresh: null };
  }
  weeks.set(key, week);
  states.set(key, week.state);
  for (const [k, w] of weeks) {
    if (weeks.size <= WEEKS_HELD) break;
    if (w.holders > 0) continue;
    weeks.delete(k);
    states.delete(k);
  }
  return week;
}

/** A surface's hold on a week until its release. */
export interface TrackerWeekAttachment {
  readonly key: string;
  /** Reactive: the week as last read. */
  readonly current: TrackerWeekState;
  release(): void;
}

/** Holds a week until the release; `shown` is whether the surface shows
 * it now (setTrackerWeekShown after). */
export function attachTrackerWeek(ref: TrackerWeekRef, shown: boolean): TrackerWeekAttachment {
  const week = hold(ref);
  const key = trackerWeekKey(ref);
  week.holders += 1;
  week.shown = shown;
  // An attach is a showing of its own: the week returned to reads again.
  week.visible = false;
  settle(week);
  let released = false;
  return {
    key,
    get current() {
      return states.get(key) ?? week.state;
    },
    release() {
      if (released) return;
      released = true;
      week.holders -= 1;
      settle(week);
    },
  };
}

/** Whether the surface holding the week shows it. */
export function setTrackerWeekShown(key: string, shown: boolean): void {
  const week = weeks.get(key);
  if (!week) return;
  week.shown = shown;
  settle(week);
}

/** Refresh: reads the week from the tracker now; a press during a
 * refresh joins it. */
export function refreshTrackerWeek(key: string): Promise<void> {
  const week = weeks.get(key);
  return week ? refresh(week) : Promise.resolve();
}

function weeksOfDays(backend: string, connection: string, author: string, days: readonly string[]): Week[] {
  return [...weeks.values()].filter((w) => w.ref.backend === backend && w.ref.connection === connection && w.ref.author === author
    && days.some((d) => d >= w.ref.monday && d <= addDays(w.ref.monday, 6)));
}

/** A write stored these days: every week holding one that shows reads
 * the computer's copy again; one that does not reads it when it shows. */
export function rereadTrackerWorklogDays(backend: string, connection: string, author: string, days: readonly string[]): void {
  for (const week of weeksOfDays(backend, connection, author, days)) if (week.visible) void read(week, false);
}

/** A write may have changed these days without the computer's copy
 * saying so: every week holding one refreshes, now if it shows, else when
 * it shows. */
export function refreshTrackerWorklogDays(backend: string, connection: string, author: string, days: readonly string[]): void {
  for (const week of weeksOfDays(backend, connection, author, days)) {
    if (week.visible) void refresh(week);
    else week.refreshedAt = 0;
  }
}

function handleVisibilityChange(): void {
  for (const week of weeks.values()) settle(week);
}

if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', handleVisibilityChange);
}

// The computer connected again: what it holds may have changed meanwhile.
onBackendStatusChange((backend, status) => {
  if (status.status !== 'connected') return;
  for (const week of weeks.values()) {
    if (week.ref.backend !== backend) continue;
    if (week.visible) void refresh(week);
    else week.refreshedAt = 0;
  }
});

export function resetTrackerTimeForTest(): void {
  weeks.clear();
  states.clear();
}
