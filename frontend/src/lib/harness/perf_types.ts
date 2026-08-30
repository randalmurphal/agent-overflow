import type {
  BusyHistogram,
  BusySummary,
  FrameHistogram,
  FrameSummary,
  Series,
} from './perfStats';

export interface PerfStartOptions {
  longFrameMs?: number;
  /** Meter names to enable. Omitted means every available meter. An explicit empty list disables every meter. */
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
  meters: string[];
  unavailableMeters: string[];
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

export interface PerfTeardownReceipt {
  v: 1;
  kind: 'perf-teardown';
  reason: 'bridge-teardown';
  partial: true;
  runId: string;
  summary: PerfSummary;
}

export const ALL_METERS = [
  'frames',
  'busy',
  'longtask',
  'loaf',
  'layout-shift',
  'event',
  'memory',
  'dom',
] as const;
export type MeterName = (typeof ALL_METERS)[number];

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
export interface Counter {
  total: number;
  mark: number;
}

export function newCounter(): Counter {
  return { total: 0, mark: 0 };
}

export interface PerfRun {
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
