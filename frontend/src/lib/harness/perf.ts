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
// The SUMMARY, by contrast, is computed in perf_summary.ts, because
// percentiles need the whole distribution and shipping every frame time
// across the wire once a second to re-fold it on the other side would be
// pure cost.
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
  recordFrame,
} from './perfStats';
import {
  armBusyProbe,
  disarmMeters,
  heapBytes,
  observe,
  startBusyProbe,
  type LayoutShiftEntry,
} from './perf_observers';
import {
  ALL_METERS,
  newCounter,
  perfMeterNames,
  unknownPerfMeters,
  type MeterName,
  type PerfRun,
  type PerfSample,
  type PerfStartOptions,
  type PerfSummary,
  type PerfTeardownReceipt,
} from './perf_types';
import { collectPerfSampleForRun, summarizePerfRun } from './perf_summary';

export { perfMeterNames, unknownPerfMeters } from './perf_types';
export type { PerfSample, PerfStartOptions, PerfSummary, PerfTeardownReceipt } from './perf_types';

/**
 * A run left armed with no matching stop keeps the rAF loop firing for the
 * life of the page, and this rig exists to hunt idle-memory bugs — a meter
 * that prevents idle is a meter that invalidates its own experiment. The
 * backend collects at least once a second while a run is live, so five
 * minutes of silence means the caller is gone.
 */
export const PERF_WATCHDOG_MS = 5 * 60_000;

let run: PerfRun | null = null;
let lastTeardownReceipt: PerfTeardownReceipt | null = null;

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

/** Arms the meters. Re-arming replaces the previous run rather than stacking observers. */
export function startPerfRun(opts: PerfStartOptions = {}): PerfSummary | null {
  const requested = opts.meters === undefined ? null : new Set(opts.meters);
  if (requested !== null) {
    const unknown = unknownPerfMeters([...requested]);
    if (unknown.length > 0) {
      throw new Error(
        `unknown perf meter${unknown.length > 1 ? 's' : ''} ${unknown
          .map((name) => JSON.stringify(name)).join(', ')} (allowed: ${perfMeterNames().join(', ')})`,
      );
    }
  }
  // Validate the complete request before disarming the current run. A bad
  // start must not destroy a valid instrument that a caller may still stop.
  const previous = run ? stopPerfRun() : null;
  selfDisarmed = null;
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
      if (wantsBusy && !armBusyProbe(state, (candidate) => run === candidate)) state.unavailable.add('busy');
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

  if (meters.has('longtask')) {
    observe(state, 'longtask', 'longtask', (entries) => {
      for (const entry of entries) {
        state.longTasks.total += 1;
        if (entry.duration > state.longestTaskMs) state.longestTaskMs = entry.duration;
      }
    });
  }
  if (meters.has('loaf')) {
    observe(state, 'loaf', 'long-animation-frame', (entries) => {
      for (const entry of entries) {
        state.loaf.total += 1;
        if (entry.duration > state.longestLoafMs) state.longestLoafMs = entry.duration;
      }
    });
  }
  if (meters.has('layout-shift')) {
    observe(state, 'layout-shift', 'layout-shift', (entries) => {
      for (const entry of entries as LayoutShiftEntry[]) {
        // Shifts within 500ms of a real input are user-caused and expected;
        // web-vitals excludes them and so does this, or every click a spec
        // makes would score as instability.
        if (entry.hadRecentInput) continue;
        state.layoutShift.total += entry.value ?? 0;
      }
    });
  }
  if (meters.has('event')) {
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
  }

  if (meters.has('memory') && heapBytes() === 0) state.unavailable.add('memory');
  // A first reading so a run stopped before its first collect still
  // carries a level rather than a zero.
  collectPerfSample();
  return previous;
}


/** One backend tick's worth of numbers. */
export function collectPerfSample(): PerfSample {
  const now = typeof performance !== 'undefined' ? performance.now() : 0;
  return collectPerfSampleForRun(run, now, armWatchdog);
}

/** Disarms the meters and returns the run summary. Null when nothing was armed. */
export function stopPerfRun(): PerfSummary | null {
  const state = run;
  if (!state) return null;
  const finalNow = typeof performance !== 'undefined' ? performance.now() : 0;
  const durationNow = typeof performance !== 'undefined' ? performance.now() : finalNow;
  const duration = Math.max(0, durationNow - state.startedAt);
  const summary = summarizePerfRun(state, finalNow, duration);
  run = null;
  disarmMeters(state);
  return summary;
}

/**
 * Stops an armed run for bridge teardown and retains the typed partial receipt
 * until the next teardown. The ordinary stop path remains unchanged because
 * it is the backend-owned completion boundary. Teardown has no reply channel,
 * so dropping this summary would make a page close indistinguishable from a
 * run that never collected any data.
 */
export function stopPerfRunForTeardown(): PerfTeardownReceipt | null {
  const runId = perfRunId();
  const summary = stopPerfRun();
  if (summary === null) return null;
  lastTeardownReceipt = {
    v: 1,
    kind: 'perf-teardown',
    reason: 'bridge-teardown',
    partial: true,
    runId,
    summary,
  };
  return lastTeardownReceipt;
}

export function lastPerfTeardownReceipt(): PerfTeardownReceipt | null {
  return lastTeardownReceipt;
}
