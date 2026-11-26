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
// The range picked last for each dashboard and Team or Mine are saved for
// the next screen (frontendStorage).

import { untrack } from 'svelte';
import type {
  Answer,
  DashboardView,
  DrillAnswer,
  Name,
  Request,
} from '../../../bindings/agent-overflow/internal/trackerdash/models';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { createRefreshScheduler } from '../utils/refreshScheduler';
import { EvaluateTrackerDashboard, TrackerDashboardDrill, TrackerDashboards } from './bindings';
import { readFrontendValue, writeFrontendValue } from './frontendStorage';

const RANGE_KEY = 'issues-dashboard-range';
const MINE_KEY = 'issues-dashboard-mine';

/** The least time between two evaluations a board's changes ask for. */
export const DASHBOARD_CHANGE_INTERVAL_MS = 5_000;
/** How often an open dashboard evaluates with no change. */
export const DASHBOARD_REFRESH_MS = 10 * 60_000;

/** The board a dashboard counts: its subscription on its computer, and
 * how many times it changed (TrackerBoardState.changes). */
export interface DashboardSource {
  readonly backend: BackendKey;
  readonly subscription: string;
  readonly changes: number;
}

/** A number of a widget whose rows a drill lists: the widget's number
 * (no cell), or the bar, point or table cell picked (its bucket's key,
 * which is empty for Unassigned and No epic, and its series, empty for
 * the whole bucket). */
export interface DrillTarget {
  readonly widget: string;
  readonly cell?: { readonly bucket: string; readonly series: string };
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

export interface TrackerDashboard {
  /** The dashboards the board offers; null until read; 'older' for a
   * computer whose release has none. Reactive. */
  readonly dashboards: readonly DashboardView[] | 'older' | null;
  readonly dashboardsError: unknown;
  /** The dashboard shown. Reactive. */
  readonly dashboard: DashboardView | null;
  readonly range: string;
  readonly mine: boolean;
  readonly person: Name | null;
  readonly epic: Name | null;
  /** The answer the screen draws; null until the first lands. */
  readonly answer: Answer | null;
  /** An evaluation of what is asked now is out, or the answer is of
   * something else (a range or narrowing since changed). */
  readonly counting: boolean;
  /** The last evaluation failed; null after one succeeds. */
  readonly error: unknown;
  readonly drill: DrillState | null;
  setRange(range: string): void;
  setMine(mine: boolean): void;
  narrowPerson(person: Name | null): void;
  narrowEpic(epic: Name | null): void;
  /** Evaluates now, as Refresh asks. */
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

function timeZone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
}

export function createTrackerDashboard(opts: { source: () => DashboardSource | null; shown: () => boolean }): TrackerDashboard {
  let dashboards = $state.raw<readonly DashboardView[] | 'older' | null>(null);
  let dashboardsError = $state.raw<unknown>(null);
  let listedFor = '';
  let rangePick = $state<string>('');
  let mine = $state(readFrontendValue(MINE_KEY) === true);
  let person = $state.raw<Name | null>(null);
  let epic = $state.raw<Name | null>(null);
  let answer = $state.raw<Answer | null>(null);
  let answerKey = $state('');
  let inFlightKey = $state('');
  let error = $state.raw<unknown>(null);
  let drill = $state.raw<DrillState | null>(null);
  let drillGen = 0;

  const dashboard: DashboardView | null = $derived(dashboards === null || dashboards === 'older' ? null : (dashboards[0] ?? null));
  const range = $derived.by(() => {
    const d = dashboard;
    if (!d) return '';
    const pick = rangePick || savedRanges()[d.id];
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
        error = null;
      } catch (err) {
        if (token.isCurrent()) error = err;
      } finally {
        if (token.isCurrent()) inFlightKey = '';
      }
    },
  });

  const stop = $effect.root(() => {
    // The dashboards the board offers, once per subscription.
    $effect(() => {
      const s = opts.source();
      if (!s?.subscription || !opts.shown()) return;
      const key = `${s.backend}\n${s.subscription}`;
      if (key === untrack(() => listedFor)) return;
      listedFor = key;
      void (async () => {
        try {
          const list = await withBackendTarget(s.backend, () => TrackerDashboards(s.subscription));
          if (listedFor !== key) return;
          dashboards = list ?? 'older';
          dashboardsError = null;
        } catch (err) {
          if (listedFor !== key) return;
          listedFor = '';
          dashboardsError = err;
        }
      })();
    });

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
  });

  async function readDrill(target: DrillTarget, page: number, count: number): Promise<void> {
    const s = opts.source();
    const r = request();
    if (!s || !r) return;
    const gen = ++drillGen;
    drill = { target, page, answer: drill?.target === target ? drill.answer : null, loading: true, error: null };
    try {
      const a = await withBackendTarget(s.backend, () =>
        TrackerDashboardDrill(s.subscription, { ...r, widget: target.widget, cell: target.cell ? { ...target.cell } : null, page, count }),
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
  }

  return {
    get dashboards() { return dashboards; },
    get dashboardsError() { return dashboardsError; },
    get dashboard() { return dashboard; },
    get range() { return range; },
    get mine() { return mine; },
    get person() { return person; },
    get epic() { return epic; },
    get answer() { return answer; },
    get counting() { return inFlightKey !== '' || (answer !== null && answerKey !== requestKey); },
    get error() { return error; },
    get drill() { return drill; },
    setRange(next) {
      const d = dashboard;
      if (!d) return;
      rangePick = next;
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
      scheduler.reset();
      scheduler.request({ immediate: true });
    },
    openDrill(target) {
      void readDrill(target, 0, 0);
    },
    drillPage(page) {
      const d = drill;
      if (!d?.answer) return;
      void readDrill(d.target, page, d.answer.count);
    },
    closeDrill,
    destroy() {
      closeDrill();
      scheduler.dispose();
      stop();
    },
  };
}
