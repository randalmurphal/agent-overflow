// The happy-dom projects fake time, so a test there has no reason to wait on
// the wall clock. One that does (a sleep past a product timer, a fake clock
// that misses the timer the code uses, a poll that waits out its interval)
// runs at that timer's speed on every run, and nothing failed it: such
// tests were most of the unit suite's idle time.
//
// For each test this reads how long the test's thread slept: wall time less
// the time the thread ran and the time it waited for a CPU, from Linux's
// per-thread scheduler counters. A loaded host makes a test wait for a CPU,
// which does not count. A file whose tests slept more than
// FILE_SLEEP_BUDGET_MS fails, naming them. A child process a test waits on
// is work, not sleep: run it through runNodeWorkload, which credits it.
// Where the counters do not exist (macOS), nothing is measured.
import { readFileSync } from 'node:fs';
import { afterAll, afterEach, beforeEach, expect } from 'vitest';

/** The most a file's tests may sleep in all: a third above the 3.0 s the
 * sleepiest file slept across three full runs on a loaded host, and below
 * the 5.6 to 16 s the files it was written for slept. */
export const FILE_SLEEP_BUDGET_MS = 4000;
const REPORTED_TESTS = 5;
const SCHEDSTAT = '/proc/thread-self/schedstat';

// Captured at import: fake timers can replace process.hrtime.
const wallNs = process.hrtime.bigint.bind(process.hrtime);

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

const CREDIT = Symbol.for('agent-overflow.test.realTimeCredit');
type CreditHolder = { [CREDIT]?: number };

/** Counts `ms` of a test's wall time as work: a child process it waited on. */
export function creditChildProcessTime(ms: number): void {
  const holder = globalThis as CreditHolder;
  holder[CREDIT] = (holder[CREDIT] ?? 0) + ms;
}

function takeCredit(): number {
  const holder = globalThis as CreditHolder;
  const credit = holder[CREDIT] ?? 0;
  holder[CREDIT] = 0;
  return credit;
}

/** Milliseconds a test slept, from its wall and scheduled time. */
export function sleptMs(wallMs: number, scheduledMs: number, creditMs: number): number {
  return Math.max(0, wallMs - scheduledMs - creditMs);
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

/** Starts measuring this thread's sleep; the result reads the milliseconds
 * slept since, less credited child process time. Null off Linux. */
export function startSleepClock(): (() => number) | null {
  const scheduled = scheduledNs();
  if (scheduled === null) return null;
  const wall = wallNs();
  takeCredit();
  return () => sleptMs(Number(wallNs() - wall) / 1e6, Number(scheduledNs()! - scheduled) / 1e6, takeCredit());
}

export function installRealTimeBudget(): void {
  if (scheduledNs() === null) return;
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
