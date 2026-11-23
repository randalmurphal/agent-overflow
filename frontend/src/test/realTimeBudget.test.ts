import { describe, expect, it } from 'vitest';
import { creditChildProcessTime, FILE_SLEEP_BUDGET_MS, sleepOverBudget, sleptMs, startSleepClock } from './realTimeBudget';

describe('the real-time budget', () => {
  it('counts wall time the thread neither ran nor waited for a CPU, less child process time', () => {
    expect(sleptMs(500, 120, 0)).toBe(380);
    expect(sleptMs(500, 120, 300)).toBe(80);
    expect(sleptMs(500, 520, 0)).toBe(0);
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
  it.runIf(startSleepClock() !== null)('reads a real wait as sleep and computing as none', async () => {
    const waited = startSleepClock()!;
    await new Promise((resolve) => setTimeout(resolve, 120));
    expect(waited()).toBeGreaterThanOrEqual(80);

    const computed = startSleepClock()!;
    let sum = 0;
    for (const end = performance.now() + 60; performance.now() < end;) sum += Math.sqrt(sum + 1);
    expect(sum).toBeGreaterThan(0);
    expect(computed()).toBeLessThan(30);

    const credited = startSleepClock()!;
    await new Promise((resolve) => setTimeout(resolve, 120));
    creditChildProcessTime(120);
    expect(credited()).toBeLessThan(40);
  });
});
