// In-page perf meters (§5 of docs/specs/testing-harness.md). Armed by
// HarnessPerfStart through the ui-query channel, polled once per backend
// tick, and summarised on HarnessPerfStop.
//
// THE BACKEND OWNS THE CADENCE. This module holds no timer of its own
// except the rAF loop that IS the frame meter; every sample is pulled by
// a `perf/collect` query. Two reasons, both learned the hard way from
// two-clock instruments:
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

import {
  DEFAULT_LONG_FRAME_MS,
  newFrameHistogram,
  newSeries,
  recordFrame,
  recordSeries,
  round2,
  summarizeFrames,
  type FrameHistogram,
  type FrameSummary,
  type Series,
} from './perfStats';

export interface PerfStartOptions {
  longFrameMs?: number;
  /** Meter names to enable. Absent/empty means every available meter. */
  meters?: readonly string[];
}

export interface PerfSample {
  v: 1;
  atMs: number;
  sinceLastMs: number;
  /** fps over the window since the previous collect, not since run start. */
  fps: number;
  frames: number;
  longFrames: number;
  maxFrameMs: number;
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
  meters: string[];
  unavailableMeters: string[];
  frames: FrameSummary;
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

const ALL_METERS = ['frames', 'longtask', 'loaf', 'layout-shift', 'event', 'memory', 'dom'] as const;
type MeterName = (typeof ALL_METERS)[number];

interface PerfRun {
  startedAt: number;
  lastCollectAt: number;
  meters: Set<MeterName>;
  unavailable: Set<MeterName>;
  hist: FrameHistogram;
  rafId: number | null;
  lastFrameAt: number | null;
  observers: PerformanceObserver[];
  // Cumulative counters. Per-sample numbers are deltas against the
  // snapshot taken at the previous collect, which is what makes a sample
  // answer "what happened in the last second" rather than "since boot".
  frameMark: number;
  longFrameMark: number;
  longTasks: number;
  longTaskMark: number;
  longestTaskMs: number;
  loaf: number;
  loafMark: number;
  longestLoafMs: number;
  layoutShift: number;
  layoutShiftMark: number;
  slowEvents: number;
  slowEventMark: number;
  worstEventMs: number;
  maxFrameMark: number;
  domNodes: Series;
  heapBytes: Series;
  panes: Map<string, Series>;
  samples: number;
}

let run: PerfRun | null = null;

export function perfRunActive(): boolean {
  return run !== null;
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

/** Arms the meters. Re-arming replaces the previous run rather than stacking observers. */
export function startPerfRun(opts: PerfStartOptions = {}): PerfSummary | null {
  const previous = run ? stopPerfRun() : null;
  const requested = opts.meters && opts.meters.length > 0 ? new Set(opts.meters) : null;
  const meters = new Set<MeterName>(
    ALL_METERS.filter((name) => requested === null || requested.has(name)),
  );
  const now = performance.now();
  const state: PerfRun = {
    startedAt: now,
    lastCollectAt: now,
    meters,
    unavailable: new Set(),
    hist: newFrameHistogram(opts.longFrameMs ?? DEFAULT_LONG_FRAME_MS),
    rafId: null,
    lastFrameAt: null,
    observers: [],
    frameMark: 0,
    longFrameMark: 0,
    longTasks: 0,
    longTaskMark: 0,
    longestTaskMs: 0,
    loaf: 0,
    loafMark: 0,
    longestLoafMs: 0,
    layoutShift: 0,
    layoutShiftMark: 0,
    slowEvents: 0,
    slowEventMark: 0,
    worstEventMs: 0,
    maxFrameMark: 0,
    domNodes: newSeries(),
    heapBytes: newSeries(),
    panes: new Map(),
    samples: 0,
  };
  run = state;

  if (meters.has('frames')) {
    if (typeof requestAnimationFrame === 'function') {
      const tick = (timestamp: number): void => {
        if (run !== state) return;
        if (state.lastFrameAt !== null) recordFrame(state.hist, timestamp - state.lastFrameAt);
        state.lastFrameAt = timestamp;
        state.rafId = requestAnimationFrame(tick);
      };
      state.rafId = requestAnimationFrame(tick);
    } else {
      state.unavailable.add('frames');
    }
  }

  observe(state, 'longtask', 'longtask', (entries) => {
    for (const entry of entries) {
      state.longTasks += 1;
      if (entry.duration > state.longestTaskMs) state.longestTaskMs = entry.duration;
    }
  });
  observe(state, 'loaf', 'long-animation-frame', (entries) => {
    for (const entry of entries) {
      state.loaf += 1;
      if (entry.duration > state.longestLoafMs) state.longestLoafMs = entry.duration;
    }
  });
  observe(state, 'layout-shift', 'layout-shift', (entries) => {
    for (const entry of entries as LayoutShiftEntry[]) {
      // Shifts within 500ms of a real input are user-caused and expected;
      // web-vitals excludes them and so does this, or every click a spec
      // makes would score as instability.
      if (entry.hadRecentInput) continue;
      state.layoutShift += entry.value ?? 0;
    }
  });
  observe(
    state,
    'event',
    'event',
    (entries) => {
      for (const entry of entries) {
        state.slowEvents += 1;
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
      longTasks: 0,
      longAnimationFrames: 0,
      layoutShift: 0,
      slowEvents: 0,
      domNodes: 0,
      heapBytes: 0,
      panes: [],
    };
  }
  const window = Math.max(0, now - state.lastCollectAt);
  const frames = state.hist.count - state.frameMark;
  const heap = state.meters.has('memory') ? heapBytes() : 0;
  const nodes =
    state.meters.has('dom') && typeof document !== 'undefined'
      ? document.getElementsByTagName('*').length
      : 0;
  const panes = countPaneRows();

  recordSeries(state.heapBytes, heap);
  recordSeries(state.domNodes, nodes);
  for (const pane of panes) {
    let series = state.panes.get(pane.paneId);
    if (!series) {
      series = newSeries();
      state.panes.set(pane.paneId, series);
    }
    recordSeries(series, pane.rows);
  }
  state.samples += 1;

  const sample: PerfSample = {
    v: 1,
    atMs: Math.round(now),
    sinceLastMs: Math.round(window),
    fps: window > 0 ? round2((frames * 1000) / window) : 0,
    frames,
    longFrames: state.hist.longFrames - state.longFrameMark,
    maxFrameMs: round2(Math.max(0, state.hist.maxMs - state.maxFrameMark)),
    longTasks: state.longTasks - state.longTaskMark,
    longAnimationFrames: state.loaf - state.loafMark,
    layoutShift: round2(state.layoutShift - state.layoutShiftMark),
    slowEvents: state.slowEvents - state.slowEventMark,
    domNodes: nodes,
    heapBytes: heap,
    panes,
  };

  state.lastCollectAt = now;
  state.frameMark = state.hist.count;
  state.longFrameMark = state.hist.longFrames;
  state.longTaskMark = state.longTasks;
  state.loafMark = state.loaf;
  state.layoutShiftMark = state.layoutShift;
  state.slowEventMark = state.slowEvents;
  // maxFrameMs is reported as "worst frame observed so far", so the mark
  // only ever grows; a per-window max would need the histogram reset,
  // which would destroy the run-wide percentiles.
  state.maxFrameMark = 0;
  return sample;
}

/** Disarms the meters and returns the run summary. Null when nothing was armed. */
export function stopPerfRun(): PerfSummary | null {
  const state = run;
  if (!state) return null;
  run = null;
  if (state.rafId !== null && typeof cancelAnimationFrame === 'function') {
    cancelAnimationFrame(state.rafId);
  }
  for (const observer of state.observers) {
    try {
      observer.disconnect();
    } catch {
      // A disconnected observer is the goal; a throw here changes nothing.
    }
  }
  const duration = Math.max(0, performance.now() - state.startedAt);
  return {
    v: 1,
    startedAtMs: Math.round(state.startedAt),
    durationMs: Math.round(duration),
    meters: [...state.meters].filter((name) => !state.unavailable.has(name)).sort(),
    unavailableMeters: [...state.unavailable].sort(),
    frames: summarizeFrames(state.hist, duration),
    longTasks: state.longTasks,
    longestTaskMs: round2(state.longestTaskMs),
    longAnimationFrames: state.loaf,
    longestAnimationFrameMs: round2(state.longestLoafMs),
    layoutShift: round2(state.layoutShift),
    slowEvents: state.slowEvents,
    worstEventLatencyMs: round2(state.worstEventMs),
    domNodes: state.domNodes,
    heapBytes: state.heapBytes,
    panes: [...state.panes.entries()]
      .map(([paneId, rows]) => ({ paneId, rows }))
      .sort((a, b) => a.paneId.localeCompare(b.paneId)),
    samples: state.samples,
  };
}
