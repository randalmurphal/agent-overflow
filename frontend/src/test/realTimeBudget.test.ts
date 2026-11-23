import { execFile } from 'node:child_process';
import { describe, expect, it, vi } from 'vitest';
import { FILE_SLEEP_BUDGET_MS, sleepOverBudget, sleptMs, startSleepClock, unionMs } from './realTimeBudget';

const realWait = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

describe('the real-time budget', () => {
  it('counts overlapping timer spans once', () => {
    expect(unionMs([[0, 100], [50, 150], [200, 250]])).toBe(200);
    expect(unionMs([[0, 300], [100, 200]])).toBe(300);
    expect(unionMs([])).toBe(0);
  });

  it('caps the timer wait at the time the thread was idle', () => {
    expect(sleptMs(400, 500, 120)).toBe(380);
    expect(sleptMs(100, 500, 120)).toBe(100);
    expect(sleptMs(400, 500, 520)).toBe(0);
  });

  it('fails a file only past the budget, naming its sleepiest tests first', () => {
    expect(sleepOverBudget([{ test: 'a', ms: FILE_SLEEP_BUDGET_MS }])).toBeNull();
    const over = sleepOverBudget([
      { test: 'small', ms: 10 },
      { test: 'large', ms: FILE_SLEEP_BUDGET_MS },
    ]);
    expect(over).toContain(`slept ${FILE_SLEEP_BUDGET_MS + 10} ms`);
    expect(over!.indexOf('"large"')).toBeLessThan(over!.indexOf('"small"'));
  });

  // The counters are Linux's; elsewhere the clock does not start.
  describe.runIf(startSleepClock() !== null)('on the real clock', () => {
    it('reads a wait on a real timer as sleep', async () => {
      const read = startSleepClock()!;
      await realWait(120);
      expect(read()).toBeGreaterThanOrEqual(100);
    });

    it('counts each interval of a poll and two overlapping timers once', async () => {
      const polled = startSleepClock()!;
      await new Promise<void>((resolve) => {
        let ticks = 0;
        const poll = setInterval(() => {
          if (++ticks < 3) return;
          clearInterval(poll);
          resolve();
        }, 40);
      });
      expect(polled()).toBeGreaterThanOrEqual(100);

      const overlapped = startSleepClock()!;
      await Promise.all([realWait(120), realWait(120)]);
      expect(overlapped()).toBeLessThan(200);
    });

    it('does not count a timer that is cleared or still pending', async () => {
      const read = startSleepClock()!;
      clearTimeout(setTimeout(() => {}, 10));
      const pending = setTimeout(() => {}, 10_000);
      await new Promise<void>((resolve) => execFile('sleep', ['0.15'], () => resolve()));
      clearTimeout(pending);
      expect(read()).toBe(0);
    });

    it('does not count waiting on a child process', async () => {
      const read = startSleepClock()!;
      await new Promise<void>((resolve, reject) => execFile('sleep', ['0.15'], (error) => (error ? reject(error) : resolve())));
      expect(read()).toBe(0);
    });

    it('does not count computing', () => {
      const read = startSleepClock()!;
      let sum = 0;
      for (const end = performance.now() + 60; performance.now() < end;) sum += Math.sqrt(sum + 1);
      expect(sum).toBeGreaterThan(0);
      expect(read()).toBe(0);
    });

    it('records nothing on a fake clock', async () => {
      vi.useFakeTimers();
      try {
        const read = startSleepClock()!;
        const fired = new Promise((resolve) => setTimeout(resolve, 5000));
        await vi.advanceTimersByTimeAsync(5000);
        await fired;
        expect(read()).toBe(0);
      } finally {
        vi.useRealTimers();
      }
    });
  });
});
