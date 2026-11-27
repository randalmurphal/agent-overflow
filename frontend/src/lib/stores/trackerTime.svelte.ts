// The weeks of logged time the Time screen shows (docs/specs/issues.md,
// "Time"; the reads in docs/architecture/tracker-backend.md, "Methods and
// events"). A week is one person's worklogs of seven days, Monday to
// Sunday, on one connection, read from the computer that holds it.
//
// Two reads, both of the whole week. The stored read (`TrackerWorklogs`)
// answers the computer's copy and sends nothing to the tracker; the
// refresh (`RefreshTrackerWorklogs`) reads the tracker first, stores what
// it read, then answers the copy. A refresh's `failure` is kept beside the
// worklogs until the next refresh answers; a stored read does not clear
// it.
//
// The order of answers. The computer answers calls concurrently, so
// answers arrive in any order. What must hold:
// - an older answer never replaces a newer one: every read takes a number
//   when it starts; a stored answer applies only above the number the
//   week last applied, and a refresh's answer, which holds what that
//   refresh stored, always applies and passes every read started before
//   it answered;
// - one refresh is out at a time. Refresh and a showing join it. A write
//   whose outcome leaves the week unknown (refused as stale, an answer
//   not in full, not sent, lost) and a reconnect need a read that starts
//   after it, so they queue one more refresh after the one out (one at
//   most, which they all share);
// - a stored read owed to a write that was made, a draft frame or a gap
//   (`oweStoredRead`) runs now with no refresh out, else once after the
//   last refresh out or queued ends, shared by everything that owed it
//   meanwhile, so no refresh answers over it;
// - a write that asked for a refresh learns how it ended (`WeekReread`);
// - a write that was made while its days could not be read after it is a
//   receipt on each held week showing them, until a refresh that started
//   after it answers with no failure;
// - a queued read starts only if its week still shows; a week that stopped
//   showing is due when it shows again, and what owed its stored read
//   settles with none sent.
//
// Nothing is read while the week is not shown (the pane hidden or covered,
// the window hidden). When it shows (attached shown, or shown again), the
// stored read runs, and so does a refresh unless this page refreshed the
// week less than five minutes ago. A write's refresh of a week not shown
// waits for it to show. No timer runs: a week nobody touches is read once.
//
// The five weeks used last are held, each with its last answer, so a week
// the person returns to shows at once; a sixth drops the one used least
// recently that no surface holds.
//
// The agent's draft rides the stored read (`WorklogDays.drafted`). A
// `tracker:time_entries_updated` frame (an agent drafted, a person
// changed, removed or approved entries) reads the computer's copy of each
// shown week holding one of its days again, coalesced
// (`DRAFT_REREAD_DELAY_MS`); a week not shown reads it when it shows. A
// gap on the channel reads every shown week of its computer again, and
// its recovery settles when those reads answered.

import { SvelteMap } from 'svelte/reactivity';
import type { FailureView, WorklogDays } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { RefreshTrackerWorklogs, TrackerWorklogs } from './bindings';
import { onBackendStatusChange } from './transportStatus.svelte';
import { takeWorklogWrites, type WeekReread } from './trackerTimeWrites.svelte';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { errString } from '../utils/errors';
import { documentHidden } from '../utils/pageVisibility';
import { addDays } from './trackerTimeModel';
import { takeTrackerTimeEntriesChannel } from './trackerBoardEvents';
import { createRefreshScheduler } from '../utils/refreshScheduler';

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

/** What a write made that the week does not show yet: "1h 30m logged on
 * WEB-1 for Nov 11: Saved. The week shows it after its next read: …". */
export interface WeekReceipt {
  readonly id: number;
  readonly sentence: string;
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
  readonly receipts: readonly WeekReceipt[];
}

/** A week this page refreshed less than this long ago is not refreshed
 * again when it shows. */
export const WEEK_REFRESH_AFTER_MS = 5 * 60_000;
/** The weeks held, the ones in use included. */
export const WEEKS_HELD = 5;

interface HeldReceipt extends WeekReceipt {
  /** The reads the week had started when the write was made: a refresh
   * numbered above it read the tracker after the write. */
  readonly after: number;
}

/** A stored read owed after the refreshes out, and its callers' wait. */
interface OwedRead {
  readonly read: Promise<void>;
  readonly settle: () => void;
}

/** A refresh to run after the one out, and the callers waiting on it. */
interface QueuedRefresh {
  readonly ended: Promise<WeekReread>;
  readonly settle: (ended: WeekReread) => void;
}

interface Week {
  readonly ref: TrackerWeekRef;
  state: TrackerWeekState;
  receipts: readonly HeldReceipt[];
  holders: number;
  shown: boolean;
  /** Whether the stored read and the refresh rule ran since it last
   * showed. */
  visible: boolean;
  /** When this page last sent a refresh of it (Date.now), 0 for never. */
  refreshedAt: number;
  /** The reads started; each read's number. */
  started: number;
  /** The number of the newest read applied, or passed by a refresh. */
  applied: number;
  refresh: Promise<WeekReread> | null;
  /** The number of the refresh last started. */
  refreshNumber: number;
  again: QueuedRefresh | null;
  owed: OwedRead | null;
}

const EMPTY: TrackerWeekState = { days: null, failure: null, readError: null, refreshing: false, receipts: [] };

const weeks = new Map<string, Week>();
// The state each surface reads, by key: a week's reactive box.
const states = new SvelteMap<string, TrackerWeekState>();
let nextReceipt = 1;

export function trackerWeekKey(ref: TrackerWeekRef): string {
  return `${ref.backend}\n${ref.connection}\n${ref.author}\n${ref.monday}`;
}

function update(week: Week, change: Partial<TrackerWeekState>): void {
  week.state = { ...week.state, ...change };
  const key = trackerWeekKey(week.ref);
  if (weeks.get(key) === week) states.set(key, week.state);
}

function setReceipts(week: Week, receipts: readonly HeldReceipt[]): void {
  week.receipts = receipts;
  update(week, { receipts: receipts.map(({ id, sentence }) => ({ id, sentence })) });
}

function visibleOf(week: Week): boolean {
  return week.holders > 0 && week.shown && !documentHidden();
}

function readStored(week: Week): Promise<void> {
  const { backend, connection, author, monday } = week.ref;
  const n = ++week.started;
  return withBackendTarget(backend, () => TrackerWorklogs(connection, author, monday, addDays(monday, 6))).then(
    (days) => {
      if (n <= week.applied) return;
      week.applied = n;
      update(week, { days, readError: null });
    },
    (err: unknown) => {
      if (n <= week.applied) return;
      // A dropped connection says nothing new: the screen says the
      // computer is not connected, and the reconnect refreshes.
      if (!isPassiveConnectionFailure(err)) update(week, { readError: err });
    },
  );
}

async function readRefresh(week: Week): Promise<WeekReread> {
  const { backend, connection, author, monday } = week.ref;
  const n = ++week.started;
  week.refreshNumber = n;
  try {
    const days = await withBackendTarget(backend, () => RefreshTrackerWorklogs(connection, author, monday, addDays(monday, 6)));
    week.applied = week.started;
    const failure = days.failure ?? null;
    update(week, { days, failure, readError: null });
    if (failure) return { kind: 'failed', message: failure.message };
    if (week.receipts.some((r) => r.after < n)) setReceipts(week, week.receipts.filter((r) => r.after >= n));
    return { kind: 'read' };
  } catch (err) {
    if (!isPassiveConnectionFailure(err)) update(week, { readError: err });
    return { kind: 'failed', message: errString(err) };
  }
}

/** Starts a refresh, with none out. */
function startRefresh(week: Week): Promise<WeekReread> {
  week.refreshedAt = Date.now();
  update(week, { refreshing: true });
  const run = readRefresh(week).then((ended) => {
    week.refresh = null;
    update(week, { refreshing: false });
    const again = week.again;
    week.again = null;
    if (again && week.visible) {
      // The owed read waits for this one too.
      void startRefresh(week).then(again.settle);
      return ended;
    }
    if (again) {
      // Hidden now: its showing reads the stored copy and, due, refreshes.
      week.refreshedAt = 0;
      again.settle({ kind: 'later' });
    }
    const owed = week.owed;
    week.owed = null;
    if (owed && week.visible) void readStored(week).then(owed.settle);
    else owed?.settle();
    return ended;
  });
  week.refresh = run;
  return run;
}

/** The stored read a write, a frame or a gap owes the week: now with no
 * refresh out, else after the refreshes out and queued (startRefresh),
 * one for all that owe it then. Settles when it answered, or at once for
 * a week that does not show, whose showing reads it. */
function oweStoredRead(week: Week): Promise<void> {
  if (!week.visible) return Promise.resolve();
  if (!week.refresh) return readStored(week);
  if (!week.owed) {
    let settle!: () => void;
    const read = new Promise<void>((resolve) => { settle = resolve; });
    week.owed = { read, settle };
  }
  return week.owed.read;
}

/** Refresh and a showing: joins the refresh out. */
function joinRefresh(week: Week): Promise<WeekReread> {
  return week.refresh ?? startRefresh(week);
}

/** A refresh that starts after this call: one more after the one out. */
function refreshAfter(week: Week): Promise<WeekReread> {
  if (!week.refresh) return startRefresh(week);
  if (!week.again) {
    let settle!: (ended: WeekReread) => void;
    const ended = new Promise<WeekReread>((resolve) => { settle = resolve; });
    week.again = { ended, settle };
  }
  return week.again.ended;
}

// The week shows now or stopped showing: on showing, its stored read, and
// a refresh unless one ran in the last five minutes.
function settle(week: Week): void {
  const visible = visibleOf(week);
  if (visible === week.visible) return;
  week.visible = visible;
  if (!visible) return;
  void readStored(week);
  if (Date.now() - week.refreshedAt >= WEEK_REFRESH_AFTER_MS) void joinRefresh(week);
}

function newWeek(ref: TrackerWeekRef): Week {
  return {
    ref, state: EMPTY, receipts: [], holders: 0, shown: false, visible: false, refreshedAt: 0,
    started: 0, applied: 0, refresh: null, refreshNumber: 0, again: null, owed: null,
  };
}

function hold(ref: TrackerWeekRef): Week {
  const key = trackerWeekKey(ref);
  let week = weeks.get(key);
  if (week) {
    // Re-inserted: the week used last is kept longest.
    weeks.delete(key);
  } else {
    week = newWeek(ref);
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
 * refresh joins it, unless the week holds a receipt that refresh started
 * before, which one more refresh after it reads. */
export function refreshTrackerWeek(key: string): Promise<WeekReread> {
  const week = weeks.get(key);
  if (!week) return Promise.resolve({ kind: 'later' });
  return week.receipts.some((r) => r.after >= week.refreshNumber) ? refreshAfter(week) : joinRefresh(week);
}

function weeksOfDays(backend: string, connection: string, author: string, days: readonly string[]): Week[] {
  return [...weeks.values()].filter((w) => w.ref.backend === backend && w.ref.connection === connection && w.ref.author === author
    && days.some((d) => d >= w.ref.monday && d <= addDays(w.ref.monday, 6)));
}

/** A write stored these days: every week holding one that shows reads
 * the computer's copy again, after the refreshes out if any
 * (oweStoredRead); one that does not reads it when it shows. */
export function rereadTrackerWorklogDays(backend: string, connection: string, author: string, days: readonly string[]): void {
  for (const week of weeksOfDays(backend, connection, author, days)) void oweStoredRead(week);
}

/** A write may have changed these days without the computer's copy
 * saying so: every week holding one refreshes after it, now if it shows,
 * else when it shows. Answers how the reads of the weeks shown ended: a
 * failure of one, else read; `later` when none shows. */
export async function refreshTrackerWorklogDays(backend: string, connection: string, author: string, days: readonly string[]): Promise<WeekReread> {
  const reads: Promise<WeekReread>[] = [];
  for (const week of weeksOfDays(backend, connection, author, days)) {
    if (week.visible) reads.push(refreshAfter(week));
    else week.refreshedAt = 0;
  }
  const ended = await Promise.all(reads);
  return ended.find((e) => e.kind === 'failed') ?? ended.find((e) => e.kind === 'read') ?? { kind: 'later' };
}

/** Puts a receipt on every week a surface holds that shows one of the
 * days; false when none does. */
function receiptOnDays(backend: string, connection: string, author: string, days: readonly string[], sentence: string): boolean {
  const held = weeksOfDays(backend, connection, author, days).filter((w) => w.holders > 0);
  const id = nextReceipt++;
  for (const week of held) setReceipts(week, [...week.receipts, { id, sentence, after: week.started }]);
  return held.length > 0;
}

/** The coalescing of a burst of draft frames into one read per week. */
export const DRAFT_REREAD_DELAY_MS = 100;

// The shown weeks a draft frame named since the last coalesced read.
const draftChanged = new Set<Week>();
const draftRereads = createRefreshScheduler({
  name: 'trackerTimeDraftRereads',
  delayMs: DRAFT_REREAD_DELAY_MS,
  maxWaitMs: 5 * DRAFT_REREAD_DELAY_MS,
  run: () => {
    const changed = [...draftChanged];
    draftChanged.clear();
    for (const week of changed) void oweStoredRead(week);
  },
});

takeTrackerTimeEntriesChannel({
  event(event, backend) {
    for (const week of weeksOfDays(backend, event.connection, event.author, event.days ?? [])) {
      if (week.visible) draftChanged.add(week);
    }
    if (draftChanged.size > 0) draftRereads.request();
  },
  gap(backend) {
    const reads = [...weeks.values()].filter((w) => w.ref.backend === backend && w.visible).map(oweStoredRead);
    return reads.length > 0 ? Promise.all(reads).then(() => undefined) : undefined;
  },
});

takeWorklogWrites({
  refresh: (ctx, days) => refreshTrackerWorklogDays(ctx.backend, ctx.connection, ctx.author, days),
  reread: (ctx, days) => rereadTrackerWorklogDays(ctx.backend, ctx.connection, ctx.author, days),
  receipt: (ctx, days, sentence) => receiptOnDays(ctx.backend, ctx.connection, ctx.author, days, sentence),
});

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
    if (week.visible) void refreshAfter(week);
    else week.refreshedAt = 0;
  }
});

export function resetTrackerTimeForTest(): void {
  weeks.clear();
  states.clear();
  draftChanged.clear();
  draftRereads.reset();
}
