import {
  newSeries,
  recordSeries,
  round2,
  summarizeBusy,
  summarizeFrames,
} from './perfStats';
import { heapBytes } from './perf_observers';
import type { Counter, PerfRun, PerfSample, PerfSummary } from './perf_types';

// A full DOM census walks every element. Its cost scales with the thing it is
// measuring, so tying it to a 250ms or 1s backend sample cadence makes a long
// run measure the probe. Ten seconds keeps peak visibility while the frame and
// busy meters remain continuous. Stop takes one final census.
const DOM_CENSUS_INTERVAL_MS = 10_000;

/** What this counter accumulated since the previous collect. */
function sinceMark(counter: Counter): number {
  return counter.total - counter.mark;
}

/** Closes the window: the next `sinceMark` measures from here. */
function advance(counter: Counter): void {
  counter.mark = counter.total;
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
export function collectPerfSampleForRun(
  state: PerfRun | null,
  now: number,
  refreshWatchdog: (state: PerfRun) => void,
): PerfSample {
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
      meters: [],
      unavailableMeters: [],
      longTasks: 0,
      longAnimationFrames: 0,
      layoutShift: 0,
      slowEvents: 0,
      domNodes: 0,
      heapBytes: 0,
      panes: [],
    };
  }
  refreshWatchdog(state);
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
    meters: [...state.meters].filter((name) => !state.unavailable.has(name)).sort(),
    unavailableMeters: [...state.unavailable].sort(),
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

export function summarizePerfRun(state: PerfRun, now: number, duration: number): PerfSummary {
  sampleDom(state, now, true);
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
