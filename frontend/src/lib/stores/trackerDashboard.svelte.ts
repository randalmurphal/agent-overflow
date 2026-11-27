// The Dashboards screen's state (docs/specs/issues.md, "Dashboards"): one
// per screen mounted, made by DashboardScreen.svelte and gone with it.
//
// What a dashboard shows is the board's computer's: it counts the board's
// copy in an evaluation of its own for each call (trackerdash), so nothing
// is held there. Here it holds the dashboards the board offers, the one
// answer the screen draws, the one evaluation in flight, the range, Team
// or Mine and the person or epic the page is narrowed to, and the one
// drill open. Every number the screen shows is from one answer: the
// widgets from the dashboard's, a drill's number from the drill's own
// evaluation, which also lists its rows.
//
// What the screen lays out is the list (TrackerDashboards): the dashboards,
// the proposals and why Ask cannot start a thread, read without counting,
// so the controls on them (Keep, Dismiss, Edit, Remove) are there whatever
// becomes of a count. The values are the answer's, by widget ID, shown
// only on the dashboard it counted.
//
// Freshness. The board's subscription keeps syncing while the screen
// shows (issuesPane.svelte.ts votes it shown), and the board's changes
// count (TrackerBoardState.changes) moves with each event that names rows
// or says to read again, a status-groups change among them, and with a
// gap. A move asks for an evaluation, at most one every 5 s
// (refreshScheduler); a change of what is counted (the range, Team or
// Mine, a narrowing, the subscription after a reconnect) drops the one in
// flight and evaluates at once; and every 10 minutes the screen evaluates
// again, for the numbers that count now. A hidden screen evaluates
// nothing and evaluates at once when it shows again.
//
// What the screen shows is the answer and the request it answered: a
// drill opened from it counts that request, not the controls as they are
// now, and while the answer shown is not of what is asked (stale) the grid
// takes no click. An evaluation that fails settles what it was asked: the
// screen stops counting and says why beside the last answer.
//
// The range picked last for each dashboard, the dashboard picked last for
// each board, and Team or Mine are saved for the next screen
// (frontendStorage).
//
// A board's own dashboards and the widgets agents proposed on it are the
// board's computer's too (trackerapp/dashboards.go): Keep, Dismiss and
// Remove are its methods, and every change there, from any client, is a
// `tracker:dashboards_updated` from that computer, on which the screen
// lists the dashboards and counts again. A change of that computer's
// settings lists them again too, for Ask's sentence, and so does Refresh,
// for whatever else the list was built from. A Keep onto a new
// dashboard shows it once the list read after the Keep holds it.

import { untrack } from 'svelte';
import { SvelteSet } from 'svelte/reactivity';
import type { KeepTarget, WorklogRead } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import type {
  Answer,
  DashboardView,
  DrillAnswer,
  Name,
  ProposalView,
  Request,
} from '../../../bindings/agent-overflow/internal/trackerdash/models';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { createRefreshScheduler } from '../utils/refreshScheduler';
import {
  DismissTrackerWidget,
  EvaluateTrackerDashboard,
  KeepTrackerWidget,
  ReadTrackerWorklogDays,
  RemoveTrackerWidget,
  StartTrackerDashboardThread,
  TrackerDashboardDrill,
  TrackerDashboards,
} from './bindings';
import { readFrontendValue, writeFrontendValue } from './frontendStorage';
import { errString } from '../utils/errors';
import { getSettings } from './settings.svelte';
import { openTicketThread } from './ticketWork.svelte';
import { prependThreadReply } from './threads.svelte';
import { threadRowFrameMark } from './threadRowFrames';
import type { TrackerBoardRef } from './trackerBoard.svelte';
import { onTrackerDashboardsUpdated } from './trackerBoardEvents';

const RANGE_KEY = 'issues-dashboard-range';
const MINE_KEY = 'issues-dashboard-mine';
const PICK_KEY = 'issues-dashboard-pick';

/** The least time between two evaluations a board's changes ask for. */
export const DASHBOARD_CHANGE_INTERVAL_MS = 5_000;
/** How often an open dashboard evaluates with no change. */
export const DASHBOARD_REFRESH_MS = 10 * 60_000;

/** The board a dashboard counts: the board, its subscription on its
 * computer, and how many times it changed (TrackerBoardState.changes). */
export interface DashboardSource {
  readonly backend: BackendKey;
  readonly board: TrackerBoardRef;
  readonly subscription: string;
  readonly changes: number;
}

/** A number of a widget whose rows a drill lists, one of the targets
 * trackerdash.DrillRequest names: the whole widget (no cell, no rest),
 * the bar, point or table cell picked (its bucket's key, which is empty
 * for Unassigned and No epic, and its series, empty for the whole
 * bucket), or `rest`, the buckets a bars or table widget left out. */
export interface DrillTarget {
  readonly widget: string;
  readonly cell?: { readonly bucket: string; readonly series: string };
  readonly rest?: true;
}

export interface DrillState {
  readonly target: DrillTarget;
  /** The page asked for. */
  readonly page: number;
  /** The last answer; null until the first lands. */
  readonly answer: DrillAnswer | null;
  readonly loading: boolean;
  readonly error: unknown;
}

/** The page's reads of your logged time for the widgets that count it
 * (ReadTrackerWorklogDays on the board's computer). */
export interface LoggedTimeRead {
  /** A read of days not read (WidgetValue.unread) is out, or its count
   * is: the widgets that wait on it say so. */
  readonly reading: boolean;
  /** Why the last read failed, '' when none failed. Its days are not read
   * again until the person asks. */
  readonly failure: string;
  /** Reads every day of the window again, as Read again asks. */
  retry(): void;
}

/** Why the page reads your logged time (trackerapp.WorklogRead*): its
 * widgets show, the board changed since the newest days were last read,
 * or the person asked. */
type LoggedReason = 'shown' | 'changed' | 'person';

const LOGGED_REASON_RANK: Record<LoggedReason, number> = { shown: 0, changed: 1, person: 2 };

/** Today in `zone`, YYYY-MM-DD: the day the dashboard counts to. */
function dayIn(zone: string): string {
  return new Intl.DateTimeFormat('en-CA', { timeZone: zone, year: 'numeric', month: '2-digit', day: '2-digit' }).format(new Date());
}

export interface TrackerDashboard {
  /** The dashboards the board offers; null until read; 'older' for a
   * computer whose release has none. Reactive. */
  readonly dashboards: readonly DashboardView[] | 'older' | null;
  readonly dashboardsError: unknown;
  /** The widgets agents proposed on the board, as the list has them.
   * Reactive. */
  readonly proposals: readonly ProposalView[];
  /** Why Ask your agent cannot start a thread on the board's computer;
   * '' when it can, or before the list is read. */
  readonly ask: string;
  /** The dashboard shown: the one picked last for the board, else the
   * built-in, which is first. Reactive. */
  readonly dashboard: DashboardView | null;
  /** Whether the dashboard shown is one the board's project keeps, which
   * takes widgets and edits, rather than the built-in. */
  readonly ownShown: boolean;
  /** The proposals, the dashboard widgets and the dashboards being
   * changed, by ID. Reactive. */
  readonly busy: ReadonlySet<string>;
  readonly range: string;
  readonly mine: boolean;
  readonly person: Name | null;
  readonly epic: Name | null;
  /** The answer the screen draws: the last of the dashboard shown; null
   * until one lands. */
  readonly answer: Answer | null;
  /** What is asked now has not been answered or refused yet: an
   * evaluation of it is out or about to start. */
  readonly counting: boolean;
  /** The answer shown is not of what is asked now (a range or narrowing
   * since changed): the screen dims it and takes no click on it. */
  readonly stale: boolean;
  /** The last evaluation failed; null after one succeeds. */
  readonly error: unknown;
  readonly drill: DrillState | null;
  /** The read of your logged time's days not read. Reactive. */
  readonly logged: LoggedTimeRead;
  pick(dashboard: string): void;
  /** Keeps a proposed widget on `to`, and shows that dashboard. */
  keep(proposal: string, to: KeepTarget): Promise<void>;
  dismiss(proposal: string): Promise<void>;
  /** Removes a widget from the dashboard shown. */
  removeWidget(widget: string): Promise<void>;
  setRange(range: string): void;
  setMine(mine: boolean): void;
  narrowPerson(person: Name | null): void;
  narrowEpic(epic: Name | null): void;
  /** Lists the dashboards again and evaluates now, as Refresh asks. */
  refresh(): void;
  openDrill(target: DrillTarget): void;
  drillPage(page: number): void;
  closeDrill(): void;
  destroy(): void;
}

function savedRanges(): Record<string, string> {
  const v = readFrontendValue(RANGE_KEY);
  return v && typeof v === 'object' ? (v as Record<string, string>) : {};
}

function savedPicks(): Record<string, string> {
  const v = readFrontendValue(PICK_KEY);
  return v && typeof v === 'object' ? (v as Record<string, string>) : {};
}

function boardKey(b: TrackerBoardRef): string {
  return `${b.backend}\n${b.projectId}\n${b.connection}\n${b.board}`;
}

function timeZone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
}

export function createTrackerDashboard(opts: { source: () => DashboardSource | null; shown: () => boolean }): TrackerDashboard {
  let dashboards = $state.raw<readonly DashboardView[] | 'older' | null>(null);
  let proposals = $state.raw<readonly ProposalView[]>([]);
  let ask = $state('');
  let dashboardsError = $state.raw<unknown>(null);
  let listedFor = '';
  // Moves with each change of the dashboards on the board's computer, so
  // the list is read again.
  let listGen = $state(0);
  let dashboardPick = $state<string>('');
  const busy = new SvelteSet<string>();
  // The range picked on each dashboard, by its ID: the saved picks, and
  // each one made since.
  let ranges = $state.raw<Record<string, string>>(savedRanges());
  let mine = $state(readFrontendValue(MINE_KEY) === true);
  let person = $state.raw<Name | null>(null);
  let epic = $state.raw<Name | null>(null);
  let answer = $state.raw<Answer | null>(null);
  let answerKey = $state('');
  // The dashboard the answer counted.
  let answerOf = $state('');
  // The source and request the answer shown answered, which a drill from
  // it counts.
  let answered: { source: DashboardSource; request: Request } | null = null;
  // The request last answered or refused.
  let settledKey = $state('');
  let inFlightKey = $state('');
  let error = $state.raw<unknown>(null);
  let drill = $state.raw<DrillState | null>(null);
  let drillGen = 0;
  // Your logged time (trackerdash worklogs.go, trackerapp
  // worklog_reads.go). The board's computer owns its reads: which days it
  // reads, never twice on its own, and how often it reads the newest days
  // again. The page asks while a widget that counts it shows, one call at
  // a time, and again at once while the answer says there is more:
  // ReadTrackerWorklogDays after each answer ('changed' when the board
  // changed since the newest days were last read, else 'shown' while
  // days are not read), again when the computer says a change may read
  // then (rereadIn), and 'person' on Refresh or Read again. Each call is
  // made only while that demand holds: the store alive, the screen shown,
  // such a widget on the dashboard, the same computer and connection.
  // The dashboard counts again after a read that read something.
  let worklogReading = $state(false);
  let worklogFailure = $state('');
  let loggedKey = '';
  // The connection whose reads run now, '' when none does.
  let pumpingKey = '';
  let owedReason: LoggedReason | null = null;
  // The board's changes when the newest days were last read for one.
  let changesRead = -1;
  let rereadTimer: ReturnType<typeof setTimeout> | null = null;
  let destroyed = false;

  const dashboard: DashboardView | null = $derived.by(() => {
    if (dashboards === null || dashboards === 'older') return null;
    const s = opts.source();
    const pick = dashboardPick || (s ? savedPicks()[boardKey(s.board)] : '');
    return dashboards.find((d) => d.id === pick) ?? dashboards[0] ?? null;
  });
  const shownAnswer = $derived(answer !== null && answerOf === dashboard?.id ? answer : null);
  const ownShown = $derived(dashboards !== null && dashboards !== 'older' && dashboard !== null && dashboards.indexOf(dashboard) > 0);
  const range = $derived.by(() => {
    const d = dashboard;
    if (!d) return '';
    const pick = ranges[d.id];
    return d.ranges.some((r) => r.id === pick) ? pick : d.default_range;
  });

  function request(): Request | null {
    const d = dashboard;
    if (!d) return null;
    return { dashboard: d.id, range, mine, person: person?.id ?? '', epic: epic?.id ?? '', time_zone: timeZone() };
  }
  const requestKey = $derived.by(() => {
    const s = opts.source();
    const r = request();
    return s && r ? `${s.backend}\n${s.subscription}\n${JSON.stringify(r)}` : '';
  });

  const scheduler = createRefreshScheduler({
    name: 'trackerDashboard',
    delayMs: 500,
    maxWaitMs: DASHBOARD_CHANGE_INTERVAL_MS,
    minIntervalMs: DASHBOARD_CHANGE_INTERVAL_MS,
    run: async (token) => {
      const s = opts.source();
      const r = request();
      const key = requestKey;
      if (!s || !r || !key) return;
      inFlightKey = key;
      try {
        const a = await withBackendTarget(s.backend, () => EvaluateTrackerDashboard(s.subscription, r));
        if (!token.isCurrent()) return;
        answer = a;
        answerKey = key;
        answerOf = r.dashboard;
        answered = { source: s, request: r };
        settledKey = key;
        error = null;
        worklogReading = false;
        readLoggedAfter(a);
        // An open drill of this dashboard counted in another unit (the
        // project changed what it counts in) reads its first page again
        // in this answer's: its numbers would be in the old one.
        const d = drill;
        if (d && drillOf && drillOf.request.dashboard === r.dashboard && drillOf.estimates !== a.estimates) {
          drillOf = { ...answered, estimates: a.estimates };
          void readDrill(d.target, 0, 0);
        }
      } catch (err) {
        if (!token.isCurrent()) return;
        settledKey = key;
        error = err;
        worklogReading = false;
      } finally {
        if (token.isCurrent()) inFlightKey = '';
      }
    },
  });

  const stop = $effect.root(() => {
    // The dashboards the board offers, once per subscription and again
    // after each change of them.
    $effect(() => {
      const s = opts.source();
      const gen = listGen;
      if (!s?.subscription || !opts.shown()) return;
      const key = `${s.backend}\n${s.subscription}\n${gen}`;
      if (key === untrack(() => listedFor)) return;
      untrack(() => void list(s, key));
    });

    // Whether Ask can start a thread is the board's computer's settings'
    // (which providers are turned on), so a change of them lists again.
    let settingsSeen: object | null = null;
    $effect(() => {
      const s = opts.source();
      if (!s) return;
      const current = getSettings(s.backend);
      if (settingsSeen !== null && current !== settingsSeen) untrack(() => { listGen += 1; });
      settingsSeen = current;
    });

    $effect(() => onTrackerDashboardsUpdated((backend) => {
      if (opts.source()?.backend !== backend) return;
      listGen += 1;
      if (opts.shown()) scheduler.request({ immediate: true });
    }));

    // What is counted changed, or the screen shows again: the evaluation
    // in flight is dropped and one starts now.
    $effect(() => {
      const key = requestKey;
      const shown = opts.shown();
      untrack(() => {
        scheduler.reset();
        inFlightKey = '';
        if (key && shown) scheduler.request({ immediate: true });
      });
    });

    // The board changed: evaluate, at most once every 5 s.
    let seen = -1;
    $effect(() => {
      const changes = opts.source()?.changes ?? -1;
      const shown = opts.shown();
      if (seen >= 0 && changes !== seen && shown) untrack(() => scheduler.request());
      seen = changes;
    });

    $effect(() => {
      if (!opts.shown()) return;
      const timer = setInterval(() => scheduler.request(), DASHBOARD_REFRESH_MS);
      return () => clearInterval(timer);
    });

    // A hidden screen reads none of your logged time; it reads again from
    // its next answer when it shows.
    $effect(() => {
      if (opts.shown()) return;
      untrack(() => {
        clearRereadTimer();
        worklogReading = false;
      });
    });
  });

  // The widgets of the dashboard shown, its proposals among them, that
  // count your logged time.
  function loggedWidgets(): Set<string> {
    const ids = new Set<string>();
    const d = dashboard;
    if (!d) return ids;
    const counts = (how: { reads?: string[] | null }) => how.reads?.includes('worklogs') ?? false;
    for (const w of d.widgets) if (counts(w.how)) ids.add(w.id);
    if (!ownShown) for (const p of proposals) if (counts(p.view.how)) ids.add(p.view.id);
    return ids;
  }

  // The source whose logged time the page reads now: one while the store
  // lives, the screen shows and the dashboard shown has a widget that
  // counts it; null otherwise.
  function loggedDemand(): DashboardSource | null {
    if (destroyed || !opts.shown()) return null;
    const s = opts.source();
    return s && loggedWidgets().size > 0 ? s : null;
  }

  function loggedKeyOf(s: DashboardSource): string {
    return `${s.backend}\n${s.board.connection}`;
  }

  function clearRereadTimer(): void {
    if (rereadTimer !== null) clearTimeout(rereadTimer);
    rereadTimer = null;
  }

  // Another computer or connection: nothing of the last one's reads holds.
  function adoptLogged(s: DashboardSource): void {
    const key = loggedKeyOf(s);
    if (key === loggedKey) return;
    loggedKey = key;
    worklogFailure = '';
    worklogReading = false;
    changesRead = -1;
    owedReason = null;
    clearRereadTimer();
  }

  // Whether a widget of answer a counts your logged time, rather than
  // saying why it cannot.
  function countsLogged(a: Answer | null): boolean {
    const ids = loggedWidgets();
    return a?.widgets.some((w) => ids.has(w.id) && !w.note && !w.error) ?? false;
  }

  // What answer a asks of your logged time: nothing when no widget of it
  // counts the time (each says why instead: no time tracking, no verified
  // account, a page narrowed to another person); the newest days read
  // again when the board changed since they were and no wait for that is
  // set; else the days not read.
  function readLoggedAfter(a: Answer): void {
    const s = loggedDemand();
    if (!s || !countsLogged(a)) return;
    adoptLogged(s);
    if (changesRead !== s.changes && rereadTimer === null) void readLogged('changed');
    else if (a.widgets.some((w) => (w.unread?.length ?? 0) > 0)) void readLogged('shown');
  }

  // Reads for reason, then again while the computer says there is more,
  // one call at a time for a connection: a call while one runs is owed,
  // the strongest reason kept, and made after it. Another connection's
  // reads start at once; the last one's stops at its next answer.
  async function readLogged(reason: LoggedReason): Promise<void> {
    const first = loggedDemand();
    if (!first) return;
    adoptLogged(first);
    const key = loggedKey;
    if (pumpingKey === key) {
      if (owedReason === null || LOGGED_REASON_RANK[reason] > LOGGED_REASON_RANK[owedReason]) owedReason = reason;
      return;
    }
    pumpingKey = key;
    let read = false;
    let next: LoggedReason | null = reason;
    try {
      while (next !== null) {
        const s = loggedDemand();
        if (!s || loggedKeyOf(s) !== key) return;
        const unread = shownAnswer?.widgets.some((w) => (w.unread?.length ?? 0) > 0) ?? false;
        if (next === 'shown' && !unread) {
          next = owedReason;
          owedReason = null;
          continue;
        }
        const why: LoggedReason = next;
        if (why === 'person') worklogFailure = '';
        const changes = s.changes;
        // Only a read of days not read says so; a known number stays
        // shown while its days are read again.
        if (unread) worklogReading = true;
        let answer: WorklogRead;
        try {
          answer = await withBackendTarget(s.backend, () => ReadTrackerWorklogDays(s.board.connection, dayIn(timeZone()), why));
        } catch (err) {
          if (!destroyed && loggedKey === key) worklogFailure = errString(err);
          return;
        }
        if (destroyed || loggedKey !== key) return;
        read ||= answer.read ?? false;
        if (answer.failure) worklogFailure = answer.failure.message;
        if (why !== 'shown' && !answer.rereadIn) changesRead = changes;
        if (answer.rereadIn) {
          clearRereadTimer();
          rereadTimer = setTimeout(() => {
            rereadTimer = null;
            const s = loggedDemand();
            if (s && changesRead !== s.changes) void readLogged('changed');
          }, answer.rereadIn);
        }
        next = answer.more ? 'shown' : null;
        if (next === null && owedReason !== null) {
          next = owedReason;
          owedReason = null;
        }
      }
    } finally {
      if (pumpingKey === key) {
        pumpingKey = '';
        owedReason = null;
        if (!destroyed && loggedKey === key) {
          // What was read counts once the dashboard counts again, and the
          // widgets read on until that answer lands.
          if (read && loggedDemand()) scheduler.request({ immediate: true });
          else worklogReading = false;
        }
      }
    }
  }

  const logged: LoggedTimeRead = {
    get reading() { return worklogReading; },
    get failure() { return worklogFailure; },
    retry() {
      void readLogged('person');
    },
  };

  function list(s: DashboardSource, key: string): Promise<void> {
    listedFor = key;
    return (async () => {
      try {
        const answer = await withBackendTarget(s.backend, () => TrackerDashboards(s.subscription));
        if (listedFor !== key) return;
        dashboards = answer?.dashboards ?? 'older';
        proposals = answer?.proposals ?? [];
        ask = answer?.ask ?? '';
        dashboardsError = null;
      } catch (err) {
        if (listedFor !== key) return;
        listedFor = '';
        dashboardsError = err;
      }
    })();
  }

  // Runs one change of the board's dashboards under id, which reads busy
  // meanwhile; its failure is the caller's to show.
  async function change(id: string, call: (s: DashboardSource) => Promise<unknown>): Promise<void> {
    const s = opts.source();
    if (!s || busy.has(id)) return;
    busy.add(id);
    try {
      await withBackendTarget(s.backend, () => call(s));
    } finally {
      busy.delete(id);
    }
  }

  function pick(id: string): void {
    const s = opts.source();
    dashboardPick = id;
    if (s) writeFrontendValue(PICK_KEY, { ...savedPicks(), [boardKey(s.board)]: id });
    closeDrill();
  }

  // The source and request the open drill counts: those of the answer it
  // was opened from, for every page, and the unit that answer counted in.
  let drillOf: { source: DashboardSource; request: Request; estimates: string } | null = null;

  async function readDrill(target: DrillTarget, page: number, count: number): Promise<void> {
    if (!drillOf) return;
    const { source: s, request: r } = drillOf;
    const gen = ++drillGen;
    drill = { target, page, answer: drill?.target === target ? drill.answer : null, loading: true, error: null };
    try {
      const a = await withBackendTarget(s.backend, () =>
        TrackerDashboardDrill(s.subscription, { ...r, widget: target.widget, cell: target.cell ? { ...target.cell } : null, rest: target.rest ?? false, page, count }),
      );
      if (gen !== drillGen) return;
      drill = { target, page: a.page, answer: a, loading: false, error: null };
    } catch (err) {
      if (gen !== drillGen) return;
      drill = { target, page, answer: drill?.answer ?? null, loading: false, error: err };
    }
  }

  function closeDrill(): void {
    drillGen += 1;
    drill = null;
    drillOf = null;
  }

  return {
    get dashboards() { return dashboards; },
    get dashboardsError() { return dashboardsError; },
    get proposals() { return proposals; },
    get ask() { return ask; },
    get dashboard() { return dashboard; },
    get ownShown() { return ownShown; },
    get busy() { return busy; },
    pick,
    async keep(proposal, to) {
      let kept = '';
      await change(proposal, async (s) => { kept = await KeepTrackerWidget(s.subscription, proposal, { ...to }); });
      const s = opts.source();
      if (!kept || !s) return;
      // The new dashboard shows once a list read after the Keep holds it.
      listGen += 1;
      await list(s, `${s.backend}\n${s.subscription}\n${listGen}`);
      pick(kept);
    },
    dismiss(proposal) {
      return change(proposal, (s) => DismissTrackerWidget(s.subscription, proposal));
    },
    removeWidget(widget) {
      const d = dashboard;
      if (!d || !ownShown) return Promise.resolve();
      return change(widget, (s) => RemoveTrackerWidget(s.subscription, d.id, widget));
    },
    get range() { return range; },
    get mine() { return mine; },
    get person() { return person; },
    get epic() { return epic; },
    get answer() { return shownAnswer; },
    get counting() { return inFlightKey !== '' || (requestKey !== '' && settledKey !== requestKey); },
    get stale() { return shownAnswer !== null && answerKey !== requestKey; },
    get error() { return error; },
    get drill() { return drill; },
    logged,
    setRange(next) {
      const d = dashboard;
      if (!d) return;
      ranges = { ...ranges, [d.id]: next };
      writeFrontendValue(RANGE_KEY, { ...savedRanges(), [d.id]: next });
      closeDrill();
    },
    setMine(next) {
      mine = next;
      if (next) person = null;
      writeFrontendValue(MINE_KEY, next);
      closeDrill();
    },
    narrowPerson(next) {
      person = next;
      if (next) mine = false;
      closeDrill();
    },
    narrowEpic(next) {
      epic = next;
      closeDrill();
    },
    refresh() {
      listGen += 1;
      scheduler.reset();
      scheduler.request({ immediate: true });
      if (countsLogged(shownAnswer)) void readLogged('person');
    },
    openDrill(target) {
      if (!answered || !shownAnswer) return;
      drillOf = { ...answered, estimates: shownAnswer.estimates };
      void readDrill(target, 0, 0);
    },
    drillPage(page) {
      const d = drill;
      if (!d?.answer) return;
      void readDrill(d.target, page, d.answer.count);
    },
    closeDrill,
    destroy() {
      destroyed = true;
      clearRereadTimer();
      closeDrill();
      scheduler.dispose();
      stop();
    },
  };
}

/** Ask your agent: a new thread on the board's computer `backend`, made
 * there (StartTrackerDashboardThread) in the project of the board's
 * subscription on a provider whose agents get the tracker tools, its draft
 * naming the board for the person to finish and send, opened as a ticket's
 * Ask opens its thread. The agent answers with the tracker tools'
 * dashboard_widget, whose proposals show on the board's dashboards. */
export async function askAgentAboutDashboards(backend: BackendKey, subscription: string): Promise<void> {
  const mark = threadRowFrameMark();
  const made = await withBackendTarget(backend, () => StartTrackerDashboardThread(subscription));
  await openTicketThread(prependThreadReply(made, mark));
}
