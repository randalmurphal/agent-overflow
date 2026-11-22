import { afterEach, describe, expect, it, vi } from 'vitest';
import { waitForWithBackoff, type WaitForTimers } from './waitForBackoff';

// Timers a test runs by hand: `delays` records every recheck and deadline
// asked for, in order.
function manualTimers() {
  const pending = new Map<number, { callback: () => void; ms: number }>();
  const delays: number[] = [];
  let next = 1;
  const timers: WaitForTimers = {
    setTimeout(callback, ms) {
      delays.push(ms);
      pending.set(next, { callback, ms });
      return next++;
    },
    clearTimeout(handle) {
      pending.delete(handle as number);
    },
  };
  // Runs the earliest-asked timer that is not the deadline.
  const runRecheck = () => {
    const entry = [...pending.entries()].find(([, { ms }]) => ms <= 50);
    if (entry === undefined) throw new Error('no recheck pending');
    pending.delete(entry[0]);
    entry[1].callback();
  };
  const runDeadline = () => {
    const entry = [...pending.entries()].find(([, { ms }]) => ms > 50);
    if (entry === undefined) throw new Error('no deadline pending');
    pending.delete(entry[0]);
    entry[1].callback();
  };
  return { timers, delays, pending, runRecheck, runDeadline };
}

const unused = (() => {
  throw new Error('the backoff took a call that belongs to Vitest');
}) as unknown as typeof vi.waitFor;

afterEach(() => {
  vi.useRealTimers();
});

describe('vi.waitFor with backoff', () => {
  it('rechecks after 1, 2, 4, ... ms, up to 50 ms between checks', async () => {
    const { timers, delays, runRecheck } = manualTimers();
    let checks = 0;
    const done = waitForWithBackoff(unused, timers)(() => {
      checks += 1;
      if (checks < 10) throw new Error('not yet');
      return 'held';
    });
    for (let i = 0; i < 9; i += 1) runRecheck();
    await expect(done).resolves.toBe('held');
    // The deadline, then one recheck after each failed check.
    expect(delays).toEqual([1000, 1, 2, 4, 8, 16, 32, 50, 50, 50]);
  });

  it('resolves at the first check that holds, and leaves no timer behind', async () => {
    const { timers, pending } = manualTimers();
    await expect(waitForWithBackoff(unused, timers)(() => 7)).resolves.toBe(7);
    expect(pending.size).toBe(0);
  });

  it('keeps rechecking while an async check is out, and resolves when one holds', async () => {
    const { timers, runRecheck, pending } = manualTimers();
    let release!: () => void;
    const answers = [Promise.reject(new Error('first misses')), new Promise<void>((resolve) => { release = resolve; })];
    let calls = 0;
    const done = waitForWithBackoff(unused, timers)(() => answers[calls++]);
    await Promise.resolve();
    runRecheck();
    expect(calls).toBe(2);
    // A recheck while the second answer is out does not call again.
    runRecheck();
    expect(calls).toBe(2);
    release();
    await expect(done).resolves.toBeUndefined();
    expect(pending.size).toBe(0);
  });

  it('rejects at the deadline with the last error the check threw', async () => {
    const { timers, runRecheck, runDeadline } = manualTimers();
    let checks = 0;
    const done = waitForWithBackoff(unused, timers)(() => {
      checks += 1;
      throw new Error(`miss ${checks}`);
    }, { timeout: 5000 });
    runRecheck();
    runDeadline();
    await expect(done).rejects.toThrow('miss 2');
  });

  it('leaves an explicit interval and fake timers to Vitest', async () => {
    const { timers, delays } = manualTimers();
    const original = vi.fn(async () => 'vitest') as unknown as typeof vi.waitFor;
    const waitFor = waitForWithBackoff(original, timers);
    await expect(waitFor(() => 1, { interval: 10 })).resolves.toBe('vitest');
    vi.useFakeTimers();
    await expect(waitFor(() => 1)).resolves.toBe('vitest');
    expect(original).toHaveBeenCalledTimes(2);
    expect(delays).toEqual([]);
  });

  it('is what vi.waitFor is in this suite', () => {
    // setup.ts installs it over Vitest's own, which it keeps beside it.
    const original = (vi as unknown as Record<symbol, unknown>)[Symbol.for('agent-overflow.test.waitForBackoff')];
    expect(typeof original).toBe('function');
    expect(vi.waitFor).not.toBe(original);
  });
});
