// The happy-dom projects fake time, so a test there has no reason to wait on
// the wall clock. One that does (a sleep past a product timer, a fake clock
// that misses the timer the code uses, a poll that waits out its interval)
// runs at that timer's speed on every run, and nothing failed it: such
// tests were most of the unit suite's idle time.
//
// What counts is waiting on a real timer, measured at the timer functions.
// Once per worker, the global setTimeout and setInterval are wrapped: each
// callback that fires during a test records the span from when it was set
// (or from its interval's previous tick) to when it fired. A test's timer
// wait is the union of those spans, so timers that overlap count once. A
// timer that is cleared, or still pending when the test ends, waited on
// nothing the test saw and counts for nothing. A poll written with these
// functions (testing-library's waitFor among them) counts each interval
// it waits out. vi.waitFor's rechecks use the functions as they were before
// the wrap (realTimers below), because what such a poll waits for counts
// on its own: a real timer's span, or nothing for a module load or I/O.
// Under fake timers the global functions are the fake ones, so nothing is
// recorded.
//
// A loaded host fires a timer late while the thread waits for a CPU. So a
// test's sleep is its timer wait capped at the time its thread was idle:
// wall time less the time it ran and waited for a CPU, from Linux's
// per-thread scheduler counters. Where those counters do not exist (macOS),
// nothing is measured. A file whose tests slept more than
// FILE_SLEEP_BUDGET_MS fails, naming them.
// oxlint-disable-next-line no-restricted-imports -- reads this thread's scheduler counters in /proc, not a repository file
import { readFileSync } from 'node:fs';
import { afterAll, afterEach, beforeEach, expect } from 'vitest';

/** The most a file's tests may sleep in all: a third above the 3.0 s the
 * sleepiest file slept across three full runs on a loaded host, and below
 * the 5.6 to 16 s the files it was written for slept. */
export const FILE_SLEEP_BUDGET_MS = 4000;
const REPORTED_TESTS = 5;
const SCHEDSTAT = '/proc/thread-self/schedstat';
const STATE = Symbol.for('agent-overflow.test.realTimeBudget');

/** Nanoseconds this thread ran and waited for a CPU, or null off Linux. */
function scheduledNs(): bigint | null {
  let text: string;
  try {
    text = readFileSync(SCHEDSTAT, 'utf8');
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return null;
    throw error;
  }
  const [ran, waited] = text.trim().split(/\s+/);
  return BigInt(ran) + BigInt(waited);
}

type Span = readonly [start: number, end: number];

type TimerState = {
  /** The timer functions as they were before the wrap. */
  readonly real: Pick<typeof globalThis, 'setTimeout' | 'clearTimeout' | 'setInterval' | 'clearInterval'>;
  /** Wall milliseconds; captured before any test can fake process.hrtime. */
  readonly now: () => number;
  /** The spans recorded since the clock started, or null when stopped. */
  spans: Span[] | null;
  started: number;
};

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

function timerState(): TimerState {
  const holder = globalThis as { [STATE]?: TimerState };
  const existing = holder[STATE];
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
  holder[STATE] = state;
  return state;
}

/** The timer functions as they were before the budget wrapped them, for a
 * poll whose wait is counted by what it waits for. */
export function realTimers(): TimerState['real'] {
  const installed = (globalThis as { [STATE]?: TimerState })[STATE];
  if (installed !== undefined) return installed.real;
  const { setTimeout, clearTimeout, setInterval, clearInterval } = globalThis;
  return { setTimeout, clearTimeout, setInterval, clearInterval };
}

/** Milliseconds a test slept: its timer wait, capped at its idle time. */
export function sleptMs(timerWaitMs: number, wallMs: number, scheduledMs: number): number {
  return Math.max(0, Math.min(timerWaitMs, wallMs - scheduledMs));
}

/** The failure for a file whose tests slept past the budget, or null. */
export function sleepOverBudget(entries: readonly { readonly test: string; readonly ms: number }[]): string | null {
  const total = entries.reduce((sum, entry) => sum + entry.ms, 0);
  if (total <= FILE_SLEEP_BUDGET_MS) return null;
  const worst = [...entries].sort((a, b) => b.ms - a.ms).slice(0, REPORTED_TESTS)
    .map((entry) => `${Math.round(entry.ms)} ms in "${entry.test}"`);
  return `This file's tests slept ${Math.round(total)} ms waiting on the real clock `
    + `(budget ${FILE_SLEEP_BUDGET_MS} ms, src/test/realTimeBudget.ts). Pass the timer the code waits on `
    + `on a fake clock, or wait for the condition instead of a duration: ${worst.join('; ')}`;
}

/** Starts measuring this thread's sleep; the result stops the clock and
 * reads the milliseconds slept since. Null off Linux. One clock runs at a
 * time: starting one stops the last. */
export function startSleepClock(): (() => number) | null {
  const scheduled = scheduledNs();
  if (scheduled === null) return null;
  const state = timerState();
  const spans: Span[] = [];
  state.spans = spans;
  state.started = state.now();
  const wall = state.started;
  return () => {
    const wallMs = state.now() - wall;
    const scheduledMs = Number(scheduledNs()! - scheduled) / 1e6;
    if (state.spans === spans) state.spans = null;
    return sleptMs(unionMs(spans), wallMs, scheduledMs);
  };
}

export function installRealTimeBudget(): void {
  if (scheduledNs() === null) return;
  timerState();
  const slept: { test: string; ms: number }[] = [];
  let read: (() => number) | null = null;
  beforeEach(() => {
    read = startSleepClock();
  });
  afterEach(() => {
    if (read === null) return;
    const ms = read();
    read = null;
    if (ms > 0) slept.push({ test: expect.getState().currentTestName ?? '(unnamed)', ms });
  });
  afterAll(() => {
    const over = sleepOverBudget(slept.splice(0));
    if (over !== null) throw new Error(over);
  });
}
