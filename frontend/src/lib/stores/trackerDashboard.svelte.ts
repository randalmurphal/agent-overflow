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
// or says to read again, a status-groups change among them, with a change
// of what the project's dashboards of the board count, and with a gap. A move asks for an evaluation, at most one every 5 s
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
// Remove are its methods, and every change there, from any client, is
// told on the board's event to its subscriptions of the project
// (TrackerBoardState.dashboards), on which the screen lists the
// dashboards again; one that moved the numbers (a limit does not) moves
// the board's changes too, so it counts again. A gap moves both. A change
// of that computer's settings lists them again too, for Ask's sentence,
// and so does Refresh, for whatever else the list was built from. A Keep
// onto a new dashboard shows it once the list read after the Keep holds
// it.
//
// The board's summary row (BoardSummaryRow.svelte) is one of these too,
// made with `row`: what it counts is one dashboard made of the widgets
// pinned to the board, in the row's order (trackerdash.Request.Widgets),
// over the built-in's default range for the team, under the same
// freshness rule; the proposals are not counted. The pins are the board
// subscription's (TrackerBoardState.pins), which a pin from any client
// changes with no read. A number widget's limit is the list's (`tiles`,
// trackerapp/board_tiles.go), with the unit it applies in; a Raise or a
// Lower sends a step, which the computer applies to the limit it stores,
// so a step counts from the last one whatever this list shows yet.

import { untrack } from 'svelte';
import { SvelteSet } from 'svelte/reactivity';
import type { BoardTilesView, KeepTarget, LimitChange } from '../../../bindings/agent-overflow/internal/trackerapp/models';
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
  PinTrackerWidget,
  RemoveTrackerWidget,
  SetTrackerWidgetLimit,
  StartTrackerDashboardThread,
  TrackerDashboardDrill,
  TrackerDashboards,
} from './bindings';
import { readFrontendValue, writeFrontendValue } from './frontendStorage';
import { getSettings } from './settings.svelte';
import { openTicketThread } from './ticketWork.svelte';
import { prependThreadReply } from './threads.svelte';
import { threadRowFrameMark } from './threadRowFrames';
import type { TrackerBoardRef } from './trackerBoard.svelte';

const RANGE_KEY = 'issues-dashboard-range';
const MINE_KEY = 'issues-dashboard-mine';
const PICK_KEY = 'issues-dashboard-pick';

/** The least time between two evaluations a board's changes ask for. */
export const DASHBOARD_CHANGE_INTERVAL_MS = 5_000;
/** How often an open dashboard evaluates with no change. */
export const DASHBOARD_REFRESH_MS = 10 * 60_000;

/** The board a dashboard counts: the board, its subscription on its
 * computer, how many times it changed (TrackerBoardState.changes) and how
 * many times its project's dashboards of it did
 * (TrackerBoardState.dashboards). */
export interface DashboardSource {
  readonly backend: BackendKey;
  readonly board: TrackerBoardRef;
  readonly subscription: string;
  readonly changes: number;
  readonly dashboards: number;
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

export interface TrackerDashboard {
  /** The dashboards the board offers; null until read; 'older' for a
   * computer whose release has none. Reactive. */
  readonly dashboards: readonly DashboardView[] | 'older' | null;
  readonly dashboardsError: unknown;
  /** Each number widget's limit with its unit, the proposals asked for
   * the board's summary row and how many widgets the row holds, as the
   * list has them; null until read, and from a computer whose release has
   * no dashboards. Reactive. */
  readonly tiles: BoardTilesView | null;
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
  pick(dashboard: string): void;
  /** Keeps a proposed widget on `to`, and shows that dashboard. */
  keep(proposal: string, to: KeepTarget): Promise<void>;
  dismiss(proposal: string): Promise<void>;
  /** Removes a widget from the dashboard shown. */
  removeWidget(widget: string): Promise<void>;
  /** Changes a number widget's limit (trackerapp.LimitChange), or clears
   * it with null. */
  setLimit(widget: string, change: LimitChange | null): Promise<void>;
  /** Pins a widget to the board's summary row, or takes it off. */
  pin(widget: string, pinned: boolean): Promise<void>;
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

/** Shows `dashboard` the next time the board's Dashboards screen opens,
 * as a pick there does, scrolled to `widget`. */
export function pickDashboardNext(board: TrackerBoardRef, dashboard: string, widget: string): void {
  writeFrontendValue(PICK_KEY, { ...savedPicks(), [boardKey(board)]: dashboard });
  shownWidget = { board: boardKey(board), widget };
}

// The widget the next Dashboards screen of a board scrolls to, once.
let shownWidget: { board: string; widget: string } | null = null;

/** The widget the board's Dashboards screen opens scrolled to, which a
 * tile of the board's summary row asked for; '' for none. Asked once. */
export function takeShownWidget(board: TrackerBoardRef): string {
  const w = shownWidget;
  if (!w || w.board !== boardKey(board)) return '';
  shownWidget = null;
  return w.widget;
}

export function resetShownWidgetForTest(): void {
  shownWidget = null;
}

function boardKey(b: TrackerBoardRef): string {
  return `${b.backend}\n${b.projectId}\n${b.connection}\n${b.board}`;
}

function timeZone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
}

/** The board's changes ask `scheduler` for a count, at most as often as
 * it allows, and every DASHBOARD_REFRESH_MS it asks for one with no
 * change, while `shown`. The rule every reader of a board's numbers
 * recounts by; call it inside an effect root. */
export function recountOnChanges(scheduler: { request(): void }, changes: () => number, shown: () => boolean): void {
  let seen = -1;
  $effect(() => {
    const n = changes();
    const on = shown();
    if (seen >= 0 && n !== seen && on) untrack(() => scheduler.request());
    seen = n;
  });
  $effect(() => {
    if (!shown()) return;
    const timer = setInterval(() => scheduler.request(), DASHBOARD_REFRESH_MS);
    return () => clearInterval(timer);
  });
}

/** The request a board's summary row counts: the widgets pinned to it on
 * `builtin`, the board's built-in dashboard, over its default range. */
export function rowRequest(builtin: DashboardView, pins: readonly string[]): Request {
  return { dashboard: builtin.id, range: builtin.default_range, mine: false, person: '', epic: '', time_zone: timeZone(), widgets: [...pins] };
}

export function createTrackerDashboard(opts: {
  source: () => DashboardSource | null;
  shown: () => boolean;
  /** The widgets pinned to the board's summary row, for the row's store:
   * it counts those alone. */
  row?: () => readonly string[];
}): TrackerDashboard {
  let dashboards = $state.raw<readonly DashboardView[] | 'older' | null>(null);
  let tiles = $state.raw<BoardTilesView | null>(null);
  let proposals = $state.raw<readonly ProposalView[]>([]);
  let ask = $state('');
  let dashboardsError = $state.raw<unknown>(null);
  let listedFor = '';
  // Moves with each change here that the list must follow: a Keep, a
  // change of the computer's settings, a Refresh.
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

  const dashboard: DashboardView | null = $derived.by(() => {
    if (dashboards === null || dashboards === 'older') return null;
    if (opts.row) return dashboards[0] ?? null;
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
    if (opts.row) {
      const pins = opts.row();
      return pins.length > 0 ? rowRequest(d, pins) : null;
    }
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
      const key = `${s.backend}\n${s.subscription}\n${s.dashboards}\n${gen}`;
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
    recountOnChanges(scheduler, () => opts.source()?.changes ?? -1, opts.shown);
  });

  function list(s: DashboardSource, key: string): Promise<void> {
    listedFor = key;
    return (async () => {
      try {
        const answer = await withBackendTarget(s.backend, () => TrackerDashboards(s.subscription));
        if (listedFor !== key) return;
        dashboards = answer?.dashboards ?? 'older';
        proposals = answer?.proposals ?? [];
        tiles = answer?.tiles ?? null;
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
  // meanwhile; its failure is the caller's to show. Whether it ran: one
  // under the same id is still out.
  async function change(id: string, call: (s: DashboardSource) => Promise<unknown>): Promise<boolean> {
    const s = opts.source();
    if (!s || busy.has(id)) return false;
    busy.add(id);
    try {
      await withBackendTarget(s.backend, () => call(s));
      return true;
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
    get tiles() { return tiles; },
    get proposals() { return proposals; },
    get ask() { return ask; },
    get dashboard() { return dashboard; },
    get ownShown() { return ownShown; },
    get busy() { return busy; },
    pick,
    async keep(proposal, to) {
      let kept = '';
      if (!await change(proposal, async (s) => { kept = await KeepTrackerWidget(s.subscription, proposal, { ...to }); })) return;
      const s = opts.source();
      if (!kept || !s) return;
      // The new dashboard shows once a list read after the Keep holds it.
      listGen += 1;
      await list(s, `${s.backend}\n${s.subscription}\n${s.dashboards}\n${listGen}`);
      pick(kept);
    },
    async dismiss(proposal) {
      await change(proposal, (s) => DismissTrackerWidget(s.subscription, proposal));
    },
    async removeWidget(widget) {
      const d = dashboard;
      if (!d || !ownShown) return;
      await change(widget, (s) => RemoveTrackerWidget(s.subscription, d.id, widget));
    },
    async setLimit(widget, limit) {
      await change(widget, (s) => SetTrackerWidgetLimit(s.subscription, widget, limit));
    },
    async pin(widget, pinned) {
      await change(widget, (s) => PinTrackerWidget(s.subscription, widget, pinned));
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
 * dashboard_widget, whose proposals show on the board's dashboards. With
 * `rowWidget`, what the person typed in the board's summary row, the
 * draft asks for a widget that shows it, proposed for the row. */
export async function askAgentAboutDashboards(backend: BackendKey, subscription: string, rowWidget: string): Promise<void> {
  const mark = threadRowFrameMark();
  const made = await withBackendTarget(backend, () => StartTrackerDashboardThread(subscription, rowWidget));
  await openTicketThread(prependThreadReply(made, mark));
}
