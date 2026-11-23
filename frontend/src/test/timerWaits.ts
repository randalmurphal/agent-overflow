// Waits on real timers, measured at the timer functions, for the sleep
// budget (realTimeBudget.ts). Once per worker, the global setTimeout and
// setInterval are wrapped: each callback that fires while a recording runs
// records the span from when it was set (or from its interval's previous
// tick) to when it fired. Spans that overlap count once. A timer that is
// cleared, or still pending when the recording stops, counts for nothing.
// A poll written with these functions (testing-library's waitFor among
// them) counts each interval it waits out. Under fake timers the global
// functions are the fake ones, so nothing is recorded.
//
// This module imports nothing from Node, so the browser setup can read the
// unwrapped functions through it.

type Span = readonly [start: number, end: number];
type Timers = Pick<typeof globalThis, 'setTimeout' | 'clearTimeout' | 'setInterval' | 'clearInterval'>;

type TimerState = {
  /** The timer functions as they were before the wrap. */
  readonly real: Timers;
  /** Wall milliseconds; captured before any test can fake process.hrtime. */
  readonly now: () => number;
  /** The spans recorded since the recording started, or null when stopped. */
  spans: Span[] | null;
  started: number;
};

const STATE = Symbol.for('agent-overflow.test.timerWaits');

/** Total length of `spans`, counting time covered by several once. */
export function unionMs(spans: readonly Span[]): number {
  const sorted = [...spans].sort((a, b) => a[0] - b[0]);
  let total = 0;
  let end = -Infinity;
  for (const [start, stop] of sorted) {
    if (stop <= end) continue;
    total += stop - Math.max(start, end);
    end = stop;
  }
  return total;
}

function installed(): TimerState | undefined {
  return (globalThis as { [STATE]?: TimerState })[STATE];
}

function wrapped(): TimerState {
  const existing = installed();
  if (existing !== undefined) return existing;
  const hrtime = process.hrtime.bigint.bind(process.hrtime);
  const state: TimerState = {
    real: {
      setTimeout: globalThis.setTimeout,
      clearTimeout: globalThis.clearTimeout,
      setInterval: globalThis.setInterval,
      clearInterval: globalThis.clearInterval,
    },
    now: () => Number(hrtime()) / 1e6,
    spans: null,
    started: 0,
  };
  const record = (set: number): void => {
    if (state.spans === null) return;
    state.spans.push([Math.max(set, state.started), state.now()]);
  };
  const { setTimeout: realSetTimeout, setInterval: realSetInterval } = state.real;
  globalThis.setTimeout = function setTimeout(callback: unknown, delay?: number, ...args: unknown[]) {
    if (typeof callback !== 'function') return realSetTimeout(callback as () => void, delay, ...args);
    const set = state.now();
    return realSetTimeout(function (this: unknown, ...passed: unknown[]) {
      record(set);
      return callback.apply(this, passed);
    }, delay, ...args);
  } as typeof globalThis.setTimeout;
  globalThis.setInterval = function setInterval(callback: unknown, delay?: number, ...args: unknown[]) {
    if (typeof callback !== 'function') return realSetInterval(callback as () => void, delay, ...args);
    let tick = state.now();
    return realSetInterval(function (this: unknown, ...passed: unknown[]) {
      record(tick);
      tick = state.now();
      return callback.apply(this, passed);
    }, delay, ...args);
  } as typeof globalThis.setInterval;
  (globalThis as { [STATE]?: TimerState })[STATE] = state;
  return state;
}

/** Wraps the timer functions, once per worker. Call it from a setup file,
 * before a shared worker records its globals. */
export function installTimerWaits(): void {
  wrapped();
}

/** The timer functions as they were before the wrap, for a poll whose wait
 * is counted by what it waits for. */
export function realTimers(): Timers {
  const state = installed();
  if (state !== undefined) return state.real;
  const { setTimeout, clearTimeout, setInterval, clearInterval } = globalThis;
  return { setTimeout, clearTimeout, setInterval, clearInterval };
}

/** Starts recording timer waits; the result stops the recording and returns its elapsed wall time and
 * timer wait. One recording runs at a time: starting one stops the last. */
export function recordTimerWaits(): () => { wallMs: number; timerWaitMs: number } {
  const state = wrapped();
  const spans: Span[] = [];
  state.spans = spans;
  const started = state.now();
  state.started = started;
  return () => {
    if (state.spans === spans) state.spans = null;
    return { wallMs: state.now() - started, timerWaitMs: unionMs(spans) };
  };
}
