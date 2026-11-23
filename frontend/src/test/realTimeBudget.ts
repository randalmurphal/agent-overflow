// The happy-dom projects fake time, so a test there has no reason to wait on
// the wall clock. One that does (a sleep past a product timer, a fake clock
// that misses the timer the code uses, a poll that waits out its interval)
// runs at that timer's speed on every run, and nothing failed it: such
// tests were most of the unit suite's idle time.
//
// What counts is waiting on a real timer (timerWaits.ts). vi.waitFor's
// rechecks use the unwrapped timer functions, because what such a poll
// waits for counts on its own: a real timer's span, or nothing for a module
// load or I/O.
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
import { installTimerWaits, recordTimerWaits } from './timerWaits';

/** The most a file's tests may sleep in all: a third above the 3.0 s the
 * sleepiest file slept across three full runs on a loaded host, and below
 * the 5.6 to 16 s the files it was written for slept. */
export const FILE_SLEEP_BUDGET_MS = 4000;
const REPORTED_TESTS = 5;
const SCHEDSTAT = '/proc/thread-self/schedstat';

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
  const stop = recordTimerWaits();
  return () => {
    const { wallMs, timerWaitMs } = stop();
    return sleptMs(timerWaitMs, wallMs, Number(scheduledNs()! - scheduled) / 1e6);
  };
}

export function installRealTimeBudget(): void {
  if (scheduledNs() === null) return;
  installTimerWaits();
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
