// In-page perf meters (§5 of docs/specs/testing-harness.md). Armed by
// HarnessPerfStart through the ui-query channel, polled once per backend
// tick, and summarised on HarnessPerfStop.
//
// THE BACKEND OWNS THE CADENCE. This module holds no timer that produces
// a sample: the rAF loop IS the frame meter and the watchdog below only
// ever disarms, so every sample is pulled by a `perf/collect` query. Two
// reasons, both learned the hard way from two-clock instruments:
//
//   1. One clock means one timeline. If the page sampled on its own
//      interval and pushed, a reader correlating a frame stall against
//      the Go heap would be interleaving two drifting sequences, and
//      "which sample goes with which" becomes a judgement call.
//   2. A missing frontend becomes a LABELLED GAP rather than silence.
//      The backend emits a `harness:perf` frame every tick regardless;
//      when the page cannot answer, the frame carries `frontendError`.
//      A push design just stops producing, which is indistinguishable
//      from a healthy idle run.
//
// The SUMMARY, by contrast, is computed here, because percentiles need
// the whole distribution and shipping every frame time across the wire
// once a second to re-fold it on the other side would be pure cost.
//
// Feature detection, never engine sniffing: WebKit has no
// `long-animation-frame` and no `performance.memory`, Chromium has both.
// Each observer is registered inside its own try/catch and a meter that
// cannot start reports itself absent instead of failing the run.
//
// TWO METERS RIDE ONE rAF LOOP, and they answer different questions.
// `frames` measures the GAP between callbacks, which a vsync-locked
// compositor quantises: under headless Chromium a 3ms tick and a 9ms tick
// both read ~16.7ms, so a gap histogram can never say whether the work fits
// a 6ms budget. `busy` measures the WORK INSIDE one tick — callback entry to
// the moment a cheap posted task runs, which is after style, layout and
// paint — and that is the number a budget is written against. LoAF is not a
// substitute: it starts reporting at 50ms, eight budgets too late.

import {
  DEFAULT_LONG_FRAME_MS,
  newBusyHistogram,
  newFrameHistogram,
  newSeries,
  recordBusy,
  recordBusyDrop,
  recordFrame,
  recordSeries,
  round2,
  summarizeBusy,
  summarizeFrames,
  type BusyHistogram,
  type BusySummary,
  type FrameHistogram,
  type FrameSummary,
  type Series,
} from './perfStats';

export interface PerfStartOptions {
  longFrameMs?: number;
  /** Meter names to enable. Absent/empty means every available meter. */
  meters?: readonly string[];
  /**
   * Main-thread budgets, in milliseconds, the busy meter reports fit
   * against. Absent/empty means DEFAULT_BUSY_BUDGETS_MS ([6, 8, 16]).
   */
  budgetsMs?: readonly number[];
  /**
   * The backend's id for this run, echoed back in the arm reply and
   * matched against the `runId` a later collect/stop carries. Empty when
   * the backend does not stamp them.
   */
  runId?: string;
}

export interface PerfSample {
  v: 1;
  atMs: number;
  sinceLastMs: number;
  /** fps over the window since the previous collect, not since run start. */
  fps: number;
  frames: number;
  longFrames: number;
  /** Worst frame IN THIS WINDOW, not the run-wide worst (that is the summary's). */
  maxFrameMs: number;
  /** Busy measurements folded IN THIS WINDOW. */
  busyTicks: number;
  /** Measurements voided in this window because the previous probe was still out. */
  busyDropped: number;
  /** Worst busy time IN THIS WINDOW. Percentiles are the summary's. */
  maxBusyMs: number;
  /** Mean busy time over this window's measurements. */
  meanBusyMs: number;
  longTasks: number;
  longAnimationFrames: number;
  layoutShift: number;
  slowEvents: number;
  domNodes: number;
  heapBytes: number;
  panes: Array<{ paneId: string; rows: number }>;
}

export interface PerfSummary {
  v: 1;
  startedAtMs: number;
  durationMs: number;
  /**
   * `performance.timeOrigin` for this document, in epoch milliseconds.
   * Every other time in this report is on the page clock, and the busy
   * meter's worst-tick list is only useful if a reader can place those
   * ticks against a Chromium trace or a wall-clock log — which needs
   * exactly this one number, reported once rather than added to every
   * entry.
   */
  timeOriginMs: number;
  meters: string[];
  unavailableMeters: string[];
  frames: FrameSummary;
  busy: BusySummary;
  longTasks: number;
  longestTaskMs: number;
  longAnimationFrames: number;
  longestAnimationFrameMs: number;
  layoutShift: number;
  slowEvents: number;
  worstEventLatencyMs: number;
  domNodes: Series;
  heapBytes: Series;
  panes: Array<{ paneId: string; rows: Series }>;
  samples: number;
}

const ALL_METERS = [
  'frames',
  'busy',
  'longtask',
  'loaf',
  'layout-shift',
  'event',
  'memory',
  'dom',
] as const;
type MeterName = (typeof ALL_METERS)[number];

// A full DOM census walks every element. Its cost scales with the thing it is
// measuring, so tying it to a 250ms or 1s backend sample cadence makes a long
// run measure the probe. Ten seconds keeps peak visibility while the frame and
// busy meters remain continuous. Stop takes one final census.
const DOM_CENSUS_INTERVAL_MS = 10_000;

function isMeterName(name: string): name is MeterName {
  return (ALL_METERS as readonly string[]).includes(name);
}

/** The meter vocabulary, for error messages and docs. */
export function perfMeterNames(): string[] {
  return [...ALL_METERS].sort();
}

/**
 * Meter names the caller asked for that this bridge does not know. A start
 * that filters to an empty set would answer `{armed:true}` and then produce
 * nothing but zeros, so an unknown name has to refuse rather than narrow.
 */
export function unknownPerfMeters(requested: readonly string[]): string[] {
  return requested.filter((name) => !isMeterName(name));
}

/**
 * A cumulative total plus the value it held at the previous collect. Every
 * per-sample number in this module is one `total - mark` delta, and having
 * that arithmetic hand-written once per counter is what let `maxFrameMark`
 * rot inside the row of near-identical lines: it was reset to 0 each
 * collect, so `hist.maxMs - mark` reported the RUN-wide worst frame under
 * a name the CLI documents as the per-sample one.
 */
interface Counter {
  total: number;
  mark: number;
}

function newCounter(): Counter {
  return { total: 0, mark: 0 };
}

/** What this counter accumulated since the previous collect. */
function sinceMark(counter: Counter): number {
  return counter.total - counter.mark;
}

/** Closes the window: the next `sinceMark` measures from here. */
function advance(counter: Counter): void {
  counter.mark = counter.total;
}

/**
 * A run left armed with no matching stop keeps the rAF loop firing for the
 * life of the page, and this rig exists to hunt idle-memory bugs — a meter
 * that prevents idle is a meter that invalidates its own experiment. The
 * backend collects at least once a second while a run is live, so five
 * minutes of silence means the caller is gone.
 */
export const PERF_WATCHDOG_MS = 5 * 60_000;

interface PerfRun {
  runId: string;
  startedAt: number;
  lastCollectAt: number;
  meters: Set<MeterName>;
  unavailable: Set<MeterName>;
  hist: FrameHistogram;
  busy: BusyHistogram;
  rafId: number | null;
  lastFrameAt: number | null;
  observers: PerformanceObserver[];
  watchdog: ReturnType<typeof setTimeout> | null;
  /**
   * The busy probe's channel, created ONCE at arm and reused for every
   * tick. Null when the busy meter is off or this engine has no
   * MessageChannel. A channel per frame would make the instrument the load.
   */
  busyChannel: MessageChannel | null;
  /**
   * The sequence number of the probe in flight. A voided probe still
   * ARRIVES — closing the port would mean building a new one per tick — so
   * its reply is recognised by carrying a stale sequence and ignored.
   */
  busySeq: number;
  busyPending: boolean;
  /** performance.now() at the pending probe's rAF-callback entry. */
  busyStartedAt: number;
  // Cumulative counters. Per-sample numbers are deltas against the
  // snapshot taken at the previous collect, which is what makes a sample
  // answer "what happened in the last second" rather than "since boot".
  // `frames`/`longFrames` mirror totals the histogram owns; the mirror is
  // taken at collect so every counter here obeys the same delta rule.
  frames: Counter;
  longFrames: Counter;
  busyTicks: Counter;
  busyDropped: Counter;
  longTasks: Counter;
  loaf: Counter;
  layoutShift: Counter;
  slowEvents: Counter;
  longestTaskMs: number;
  longestLoafMs: number;
  worstEventMs: number;
  /**
   * Worst frame time observed since the previous collect. A true per-window
   * max cannot be derived from the histogram — it folds every frame of the
   * run and resetting it would destroy the percentiles — so it is tracked
   * where the frame deltas are recorded and zeroed at each collect.
   */
  windowMaxMs: number;
  /**
   * Worst busy time since the previous collect, and the sum over that same
   * window. Same reason as windowMaxMs: the run histogram cannot be reset
   * without destroying the percentiles a stop reports.
   */
  windowBusyMaxMs: number;
  windowBusySumMs: number;
  domNodes: Series;
  heapBytes: Series;
  panes: Map<string, Series>;
  lastDomCensusAt: number | null;
  lastDomNodes: number;
  lastPaneRows: Array<{ paneId: string; rows: number }>;
  samples: number;
}

let run: PerfRun | null = null;

/**
 * Set when the watchdog disarmed a run behind the caller's back, and kept
 * until the next arm: a collect or stop that arrives afterwards must learn
 * WHY its meters are gone rather than read "no perf run is armed" and look
 * like a caller sequencing bug.
 */
let selfDisarmed: { runId: string; afterMs: number } | null = null;

export function perfRunActive(): boolean {
  return run !== null;
}

/** The id this page's run was armed with; empty when nothing is armed. */
export function perfRunId(): string {
  return run?.runId ?? '';
}

/**
 * Whether a `collect`/`stop` naming `runId` is this page's business.
 *
 * Several pages can be attached to one backend (a browser tab beside the
 * webview), and the ui-query event goes to all of them — so an UNARMED page
 * answering `{error:"no perf run is armed"}` can win the first-reply race
 * and poison the armed page's tick. A page with no run of that name stays
 * silent and lets the armed one answer.
 *
 * Two ways to be addressed anyway, and both are about tolerating a backend
 * that stamps ids in some specs and not others: an unstamped query is
 * everyone's (the pre-runId behaviour), and a page armed WITHOUT an id
 * cannot tell that a stamped query is not its own — silencing it there
 * would silence the only page that can answer at all.
 */
export function perfRunAddressed(runId: string): boolean {
  if (runId === '') return true;
  if (run !== null) return run.runId === '' || run.runId === runId;
  if (selfDisarmed === null) return false;
  return selfDisarmed.runId === '' || selfDisarmed.runId === runId;
}

/**
 * The refusal a late collect/stop is owed after the watchdog fired, or null
 * when this page has not self-disarmed since its last arm.
 */
export function perfSelfDisarmMessage(): string | null {
  if (selfDisarmed === null) return null;
  return `perf run self-disarmed after ${selfDisarmed.afterMs}ms without a collect`;
}

/**
 * Forgets a self-disarm. Called when the whole bridge goes away: the notice
 * exists for the caller of THAT run, and nothing that arrives after a
 * teardown is that caller.
 */
export function clearPerfSelfDisarm(): void {
  selfDisarmed = null;
}

interface MemoryPerformance extends Performance {
  memory?: { usedJSHeapSize?: number };
}

function heapBytes(): number {
  const memory = (performance as MemoryPerformance).memory;
  return typeof memory?.usedJSHeapSize === 'number' ? memory.usedJSHeapSize : 0;
}

function supportedEntryTypes(): readonly string[] {
  if (typeof PerformanceObserver === 'undefined') return [];
  return PerformanceObserver.supportedEntryTypes ?? [];
}

function observe(
  state: PerfRun,
  meter: MeterName,
  type: string,
  handle: (entries: PerformanceEntryList) => void,
  init: PerformanceObserverInit = {},
): void {
  if (!state.meters.has(meter)) return;
  if (!supportedEntryTypes().includes(type)) {
    state.unavailable.add(meter);
    return;
  }
  try {
    const observer = new PerformanceObserver((list) => handle(list.getEntries()));
    observer.observe({ type, buffered: false, ...init });
    state.observers.push(observer);
  } catch {
    // A type the browser lists but refuses to observe (some entry types
    // need a secure context or a flag). Absent, not fatal.
    state.unavailable.add(meter);
  }
}

interface LayoutShiftEntry extends PerformanceEntry {
  value?: number;
  hadRecentInput?: boolean;
}

/** Stops the rAF loop and disconnects the observers. Leaves `run` alone. */
function disarmMeters(state: PerfRun): void {
  if (state.watchdog !== null && typeof clearTimeout === 'function') {
    clearTimeout(state.watchdog);
  }
  state.watchdog = null;
  if (state.rafId !== null && typeof cancelAnimationFrame === 'function') {
    cancelAnimationFrame(state.rafId);
  }
  state.rafId = null;
  if (state.busyChannel !== null) {
    const channel = state.busyChannel;
    state.busyChannel = null;
    try {
      channel.port1.onmessage = null;
      channel.port1.close();
      channel.port2.close();
    } catch {
      // A closed port is the goal; a throw here changes nothing.
    }
  }
  state.busyPending = false;
  for (const observer of state.observers) {
    try {
      observer.disconnect();
    } catch {
      // A disconnected observer is the goal; a throw here changes nothing.
    }
  }
  state.observers = [];
}

/**
 * (Re)arms the silence watchdog. Every collect pushes it out, so it only
 * ever fires for a run whose caller stopped asking — a crashed spec, a
 * killed CLI, a backend that went away mid-run.
 */
function armWatchdog(state: PerfRun): void {
  if (typeof setTimeout !== 'function') return;
  if (state.watchdog !== null && typeof clearTimeout === 'function') {
    clearTimeout(state.watchdog);
  }
  state.watchdog = setTimeout(() => {
    if (run !== state) return;
    run = null;
    disarmMeters(state);
    // The summary is dropped rather than parked: nobody is left to collect
    // it, and holding a run's histogram for a caller that vanished is the
    // retention this rig is supposed to be measuring.
    selfDisarmed = { runId: state.runId, afterMs: PERF_WATCHDOG_MS };
  }, PERF_WATCHDOG_MS);
  // A watchdog must never be the reason a Node-side test process (or a
  // future SSR pass) stays alive.
  (state.watchdog as { unref?: () => void }).unref?.();
}

/**
 * Builds the busy meter's probe channel, once, for the life of the run.
 * False when this engine has none — feature detection, never engine
 * sniffing, and the run reports the meter absent rather than failing.
 *
 * The measurement is "time until the main thread could service a cheap
 * task", so the task has to BE cheap and has to be a task: a MessageChannel
 * post is the standard zero-work macrotask, and a macrotask is what the
 * engine runs only after this tick's remaining callbacks, style, layout and
 * paint are done. A microtask would resolve inside the callback and measure
 * nothing.
 */
function armBusyProbe(state: PerfRun): boolean {
  if (typeof MessageChannel !== 'function') return false;
  try {
    const channel = new MessageChannel();
    channel.port1.onmessage = (event: MessageEvent): void => {
      if (run !== state || !state.busyPending) return;
      // A probe whose tick was superseded still arrives. Its sequence is
      // stale, and the interval it would report spans two ticks.
      if (event.data !== state.busySeq) return;
      state.busyPending = false;
      const busyMs = performance.now() - state.busyStartedAt;
      // The tick's own ENTRY timestamp rides along, not the reply's: the
      // worst-tick list names when the expensive work STARTED, which is
      // where a trace has to be opened.
      recordBusy(state.busy, busyMs, state.busyStartedAt);
      if (!Number.isFinite(busyMs) || busyMs < 0) return;
      if (busyMs > state.windowBusyMaxMs) state.windowBusyMaxMs = busyMs;
      state.windowBusySumMs += busyMs;
    };
    state.busyChannel = channel;
    return true;
  } catch {
    // A channel the engine lists but refuses to build. Absent, not fatal.
    return false;
  }
}

/**
 * Posts one tick's probe. ONE measurement is in flight at a time: when the
 * previous tick's probe has not answered, the frame it would be attributed
 * to is already over, so it is DROPPED (and counted as such) rather than
 * charged to whichever tick happens to be running when it lands.
 */
function startBusyProbe(state: PerfRun, entryMs: number): void {
  const channel = state.busyChannel;
  if (channel === null) return;
  if (state.busyPending) recordBusyDrop(state.busy);
  state.busySeq += 1;
  state.busyPending = true;
  state.busyStartedAt = entryMs;
  try {
    channel.port2.postMessage(state.busySeq);
  } catch {
    // Nothing is in flight if the post never happened; leaving `pending`
    // set would drop the NEXT tick too, for a failure that was this one's.
    state.busyPending = false;
  }
}

/** Arms the meters. Re-arming replaces the previous run rather than stacking observers. */
export function startPerfRun(opts: PerfStartOptions = {}): PerfSummary | null {
  const previous = run ? stopPerfRun() : null;
  selfDisarmed = null;
  const requested = opts.meters && opts.meters.length > 0 ? new Set(opts.meters) : null;
  const meters = new Set<MeterName>(
    ALL_METERS.filter((name) => requested === null || requested.has(name)),
  );
  const now = performance.now();
  const state: PerfRun = {
    runId: opts.runId ?? '',
    startedAt: now,
    lastCollectAt: now,
    meters,
    unavailable: new Set(),
    hist: newFrameHistogram(opts.longFrameMs ?? DEFAULT_LONG_FRAME_MS),
    busy: newBusyHistogram(opts.budgetsMs),
    rafId: null,
    lastFrameAt: null,
    observers: [],
    watchdog: null,
    busyChannel: null,
    busySeq: 0,
    busyPending: false,
    busyStartedAt: 0,
    frames: newCounter(),
    longFrames: newCounter(),
    busyTicks: newCounter(),
    busyDropped: newCounter(),
    longTasks: newCounter(),
    loaf: newCounter(),
    layoutShift: newCounter(),
    slowEvents: newCounter(),
    longestTaskMs: 0,
    longestLoafMs: 0,
    worstEventMs: 0,
    windowMaxMs: 0,
    windowBusyMaxMs: 0,
    windowBusySumMs: 0,
    domNodes: newSeries(),
    heapBytes: newSeries(),
    panes: new Map(),
    lastDomCensusAt: null,
    lastDomNodes: 0,
    lastPaneRows: [],
    samples: 0,
  };
  run = state;
  armWatchdog(state);

  // One loop serves two meters. `frames` reads the GAP between callbacks and
  // `busy` reads the WORK inside one, so either alone is enough reason to
  // turn the loop, and a run that asked for only one must not pay for the
  // other: `busy` with no `frames` never touches the histogram, and `frames`
  // with no `busy` posts no probe.
  const wantsFrames = meters.has('frames');
  const wantsBusy = meters.has('busy');
  if (wantsFrames || wantsBusy) {
    if (typeof requestAnimationFrame === 'function') {
      if (wantsBusy && !armBusyProbe(state)) state.unavailable.add('busy');
      const tick = (timestamp: number): void => {
        if (run !== state) return;
        // t0 at callback ENTRY, before this loop does anything: everything
        // the tick costs — the remaining rAF callbacks, then style, layout
        // and paint — lands between here and the probe's reply.
        if (state.busyChannel !== null) startBusyProbe(state, performance.now());
        if (wantsFrames) {
          if (state.lastFrameAt !== null) {
            const deltaMs = timestamp - state.lastFrameAt;
            recordFrame(state.hist, deltaMs);
            // Same admission rule recordFrame uses: rAF timestamps go
            // backwards across a suspend/restore in more than one engine, and
            // a bogus delta must not become the window's reported worst frame.
            if (Number.isFinite(deltaMs) && deltaMs > state.windowMaxMs) {
              state.windowMaxMs = deltaMs;
            }
          }
          state.lastFrameAt = timestamp;
        }
        state.rafId = requestAnimationFrame(tick);
      };
      state.rafId = requestAnimationFrame(tick);
    } else {
      if (wantsFrames) state.unavailable.add('frames');
      if (wantsBusy) state.unavailable.add('busy');
    }
  }

  observe(state, 'longtask', 'longtask', (entries) => {
    for (const entry of entries) {
      state.longTasks.total += 1;
      if (entry.duration > state.longestTaskMs) state.longestTaskMs = entry.duration;
    }
  });
  observe(state, 'loaf', 'long-animation-frame', (entries) => {
    for (const entry of entries) {
      state.loaf.total += 1;
      if (entry.duration > state.longestLoafMs) state.longestLoafMs = entry.duration;
    }
  });
  observe(state, 'layout-shift', 'layout-shift', (entries) => {
    for (const entry of entries as LayoutShiftEntry[]) {
      // Shifts within 500ms of a real input are user-caused and expected;
      // web-vitals excludes them and so does this, or every click a spec
      // makes would score as instability.
      if (entry.hadRecentInput) continue;
      state.layoutShift.total += entry.value ?? 0;
    }
  });
  observe(
    state,
    'event',
    'event',
    (entries) => {
      for (const entry of entries) {
        state.slowEvents.total += 1;
        if (entry.duration > state.worstEventMs) state.worstEventMs = entry.duration;
      }
    },
    // 16ms is one frame at 60Hz: below that an input handler cannot have
    // cost a frame, and the buffer would fill with noise.
    { durationThreshold: 16 } as PerformanceObserverInit,
  );

  if (meters.has('memory') && heapBytes() === 0) state.unavailable.add('memory');
  // A first reading so a run stopped before its first collect still
  // carries a level rather than a zero.
  collectPerfSample();
  return previous;
}

function countPaneRows(): Array<{ paneId: string; rows: number }> {
  if (typeof document === 'undefined') return [];
  const out: Array<{ paneId: string; rows: number }> = [];
  for (const pane of document.querySelectorAll('[data-pane-id]')) {
    out.push({
      paneId: pane.getAttribute('data-pane-id') ?? '',
      rows: pane.querySelectorAll('[data-row-index]').length,
    });
  }
  return out;
}

function sampleDom(
  state: PerfRun,
  now: number,
  force = false,
): { nodes: number; panes: Array<{ paneId: string; rows: number }> } {
  if (!state.meters.has('dom') || typeof document === 'undefined') {
    return { nodes: 0, panes: [] };
  }
  const due = state.lastDomCensusAt === null ||
    now - state.lastDomCensusAt >= DOM_CENSUS_INTERVAL_MS;
  if (!due && !(force && now !== state.lastDomCensusAt)) {
    return { nodes: state.lastDomNodes, panes: state.lastPaneRows };
  }

  const nodes = document.getElementsByTagName('*').length;
  const panes = countPaneRows();
  state.lastDomCensusAt = now;
  state.lastDomNodes = nodes;
  state.lastPaneRows = panes;
  recordSeries(state.domNodes, nodes);
  for (const pane of panes) {
    let series = state.panes.get(pane.paneId);
    if (!series) {
      series = newSeries();
      state.panes.set(pane.paneId, series);
    }
    recordSeries(series, pane.rows);
  }
  return { nodes, panes };
}

/** One tick's worth of numbers. Called by the backend, once per sampleMs. */
export function collectPerfSample(): PerfSample {
  const state = run;
  const now = typeof performance !== 'undefined' ? performance.now() : 0;
  if (!state) {
    return {
      v: 1,
      atMs: Math.round(now),
      sinceLastMs: 0,
      fps: 0,
      frames: 0,
      longFrames: 0,
      maxFrameMs: 0,
      busyTicks: 0,
      busyDropped: 0,
      maxBusyMs: 0,
      meanBusyMs: 0,
      longTasks: 0,
      longAnimationFrames: 0,
      layoutShift: 0,
      slowEvents: 0,
      domNodes: 0,
      heapBytes: 0,
      panes: [],
    };
  }
  armWatchdog(state);
  const windowMs = Math.max(0, now - state.lastCollectAt);
  // The histogram owns these two totals (it has to: the percentiles fold
  // over the whole run), so they are mirrored in here, once, and then obey
  // the same delta rule as every other counter.
  state.frames.total = state.hist.count;
  state.longFrames.total = state.hist.longFrames;
  state.busyTicks.total = state.busy.count;
  state.busyDropped.total = state.busy.dropped;
  const frames = sinceMark(state.frames);
  const busyTicks = sinceMark(state.busyTicks);
  // An unavailable memory meter keeps its series EMPTY (count 0) rather
  // than recording zeros: a zero sample would survive into the bench
  // aggregate and compare 0-against-0 in a baseline, hiding the fact
  // that this engine (WebKitGTK has no performance.memory) never
  // measured anything.
  const heapMeasured = state.meters.has('memory') && !state.unavailable.has('memory');
  const heap = heapMeasured ? heapBytes() : 0;
  // The census is rate-limited independently of this sample cadence. Both
  // fields return the last exact level between censuses so live watchers keep
  // a stable value rather than mistaking "not sampled" for zero.
  const { nodes, panes } = sampleDom(state, now);

  if (heapMeasured) recordSeries(state.heapBytes, heap);
  state.samples += 1;

  const sample: PerfSample = {
    v: 1,
    atMs: Math.round(now),
    sinceLastMs: Math.round(windowMs),
    fps: windowMs > 0 ? round2((frames * 1000) / windowMs) : 0,
    frames,
    longFrames: sinceMark(state.longFrames),
    maxFrameMs: round2(state.windowMaxMs),
    busyTicks,
    busyDropped: sinceMark(state.busyDropped),
    maxBusyMs: round2(state.windowBusyMaxMs),
    meanBusyMs: busyTicks > 0 ? round2(state.windowBusySumMs / busyTicks) : 0,
    longTasks: sinceMark(state.longTasks),
    longAnimationFrames: sinceMark(state.loaf),
    layoutShift: round2(sinceMark(state.layoutShift)),
    slowEvents: sinceMark(state.slowEvents),
    domNodes: nodes,
    heapBytes: heap,
    panes,
  };

  state.lastCollectAt = now;
  state.windowMaxMs = 0;
  state.windowBusyMaxMs = 0;
  state.windowBusySumMs = 0;
  advance(state.frames);
  advance(state.longFrames);
  advance(state.busyTicks);
  advance(state.busyDropped);
  advance(state.longTasks);
  advance(state.loaf);
  advance(state.layoutShift);
  advance(state.slowEvents);
  return sample;
}

/** Disarms the meters and returns the run summary. Null when nothing was armed. */
export function stopPerfRun(): PerfSummary | null {
  const state = run;
  if (!state) return null;
  sampleDom(state, typeof performance !== 'undefined' ? performance.now() : 0, true);
  run = null;
  disarmMeters(state);
  const duration = Math.max(0, performance.now() - state.startedAt);
  return {
    v: 1,
    startedAtMs: Math.round(state.startedAt),
    durationMs: Math.round(duration),
    timeOriginMs: round2(typeof performance.timeOrigin === 'number' ? performance.timeOrigin : 0),
    meters: [...state.meters].filter((name) => !state.unavailable.has(name)).sort(),
    unavailableMeters: [...state.unavailable].sort(),
    frames: summarizeFrames(state.hist, duration),
    busy: summarizeBusy(state.busy),
    longTasks: state.longTasks.total,
    longestTaskMs: round2(state.longestTaskMs),
    longAnimationFrames: state.loaf.total,
    longestAnimationFrameMs: round2(state.longestLoafMs),
    layoutShift: round2(state.layoutShift.total),
    slowEvents: state.slowEvents.total,
    worstEventLatencyMs: round2(state.worstEventMs),
    domNodes: state.domNodes,
    heapBytes: state.heapBytes,
    panes: [...state.panes.entries()]
      .map(([paneId, rows]) => ({ paneId, rows }))
      .sort((a, b) => a.paneId.localeCompare(b.paneId)),
    samples: state.samples,
  };
}
