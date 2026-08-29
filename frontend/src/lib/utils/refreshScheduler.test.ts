import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { debounce } from './debounce';
import { createRefreshScheduler, type RefreshToken } from './refreshScheduler';

const DELAY_MS = 100;
const MAX_WAIT_MS = 400;

/** Let every pending microtask hop (the run chain is three deep) settle. */
async function flush(): Promise<void> {
  await vi.advanceTimersByTimeAsync(0);
}

describe('createRefreshScheduler', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('runs once, delayMs after a single request', async () => {
    const runs: number[] = [];
    const scheduler = createRefreshScheduler({
      name: 'test',
      delayMs: DELAY_MS,
      maxWaitMs: MAX_WAIT_MS,
      run: async () => { runs.push(Date.now()); },
    });

    scheduler.request();
    await vi.advanceTimersByTimeAsync(DELAY_MS - 1);
    expect(runs).toHaveLength(0);

    await vi.advanceTimersByTimeAsync(1);
    expect(runs).toHaveLength(1);

    // Nothing trailing behind it: one request, one run.
    await vi.advanceTimersByTimeAsync(10_000);
    expect(runs).toHaveLength(1);
    scheduler.dispose();
  });

  // THE ANTI-STARVATION PROOF, and the reason this primitive exists. Events
  // arrive closer together than delayMs and never stop; a trailing debounce
  // postpones its callback forever (asserted below on the real `debounce`),
  // while the scheduler lands exactly on the absolute deadline.
  it('fires at the maxWait deadline under a stream a trailing debounce would starve', async () => {
    const runs: number[] = [];
    const scheduler = createRefreshScheduler({
      name: 'test',
      delayMs: DELAY_MS,
      maxWaitMs: MAX_WAIT_MS,
      run: async () => { runs.push(Date.now()); },
    });
    const debounced = vi.fn();
    const starved = debounce(debounced, DELAY_MS);

    const startedAt = Date.now();
    scheduler.request();
    starved();
    // 60ms gaps: every one of them restarts a trailing debounce's timer.
    for (let i = 0; i < 15; i += 1) {
      await vi.advanceTimersByTimeAsync(60);
      scheduler.request();
      starved();
    }

    // 900ms of unbroken stream: the debounce never fired once…
    expect(debounced).not.toHaveBeenCalled();
    // …while the scheduler landed on the deadline and kept landing, never
    // leaving the surface stale for longer than one deadline plus the
    // post-completion cooldown.
    expect(runs.length).toBeGreaterThanOrEqual(2);
    expect(runs[0]! - startedAt).toBe(MAX_WAIT_MS);
    let previous = startedAt;
    for (const at of runs) {
      expect(at - previous).toBeLessThanOrEqual(MAX_WAIT_MS + DELAY_MS);
      previous = at;
    }
    starved.cancel();
    scheduler.dispose();
  });

  it('answers a request made during a run with exactly one trailing run', async () => {
    const runs: RefreshToken[] = [];
    let finish: (() => void) | null = null;
    const scheduler = createRefreshScheduler({
      name: 'test',
      delayMs: DELAY_MS,
      maxWaitMs: MAX_WAIT_MS,
      run: (token) => {
        runs.push(token);
        return new Promise<void>((resolve) => { finish = resolve; });
      },
    });

    scheduler.request({ immediate: true });
    await flush();
    expect(runs).toHaveLength(1);

    // A burst arrives while the RPC is still out. Nothing may overtake it.
    for (let i = 0; i < 20; i += 1) scheduler.request();
    await vi.advanceTimersByTimeAsync(5_000);
    expect(runs).toHaveLength(1);

    const resolveFirst = finish!;
    resolveFirst();
    await vi.advanceTimersByTimeAsync(5_000);
    expect(runs).toHaveLength(2);
    // …and exactly one, not one per request in the burst.
    await vi.advanceTimersByTimeAsync(5_000);
    expect(runs).toHaveLength(2);
    scheduler.dispose();
  });

  it('keeps scheduling after a run rejects', async () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    let calls = 0;
    const scheduler = createRefreshScheduler({
      name: 'flaky',
      delayMs: DELAY_MS,
      maxWaitMs: MAX_WAIT_MS,
      run: async () => {
        calls += 1;
        if (calls === 1) throw new Error('boom');
      },
    });

    scheduler.request({ immediate: true });
    await flush();
    expect(calls).toBe(1);
    expect(consoleError).toHaveBeenCalledWith(
      expect.stringContaining('refreshScheduler(flaky)'),
      expect.any(Error),
    );

    scheduler.request();
    await vi.advanceTimersByTimeAsync(DELAY_MS * 2);
    expect(calls).toBe(2);
    scheduler.dispose();
  });

  it('marks a run stale on reset so the new key wins whatever the response order', async () => {
    const applied: string[] = [];
    const dropped: string[] = [];
    let key = 'A';
    let releaseA: (() => void) | null = null;
    const scheduler = createRefreshScheduler({
      name: 'test',
      delayMs: DELAY_MS,
      maxWaitMs: MAX_WAIT_MS,
      run: async (token) => {
        const forKey = key;
        if (forKey === 'A') await new Promise<void>((resolve) => { releaseA = resolve; });
        if (!token.isCurrent()) {
          dropped.push(forKey);
          return;
        }
        applied.push(forKey);
      },
    });

    scheduler.request({ immediate: true });
    await flush();

    // Key switch while A's fetch is still out.
    key = 'B';
    scheduler.reset();
    scheduler.request({ immediate: true });
    await flush();
    expect(applied).toEqual(['B']);

    // A's answer lands late and must not overwrite B's.
    releaseA!();
    await flush();
    expect(applied).toEqual(['B']);
    expect(dropped).toEqual(['A']);
    scheduler.dispose();
  });

  it('drops the pending cycle on reset', async () => {
    const runs: number[] = [];
    const scheduler = createRefreshScheduler({
      name: 'test',
      delayMs: DELAY_MS,
      maxWaitMs: MAX_WAIT_MS,
      run: async () => { runs.push(Date.now()); },
    });

    scheduler.request();
    scheduler.reset();
    await vi.advanceTimersByTimeAsync(10_000);
    expect(runs).toHaveLength(0);
    scheduler.dispose();
  });

  it('runs nothing after dispose with a timer pending', async () => {
    const runs: number[] = [];
    const scheduler = createRefreshScheduler({
      name: 'test',
      delayMs: DELAY_MS,
      maxWaitMs: MAX_WAIT_MS,
      run: async () => { runs.push(Date.now()); },
    });

    scheduler.request();
    await vi.advanceTimersByTimeAsync(DELAY_MS - 1);
    scheduler.dispose();

    await vi.advanceTimersByTimeAsync(10_000);
    expect(runs).toHaveLength(0);
    // A disposed scheduler stays disposed.
    scheduler.request({ immediate: true });
    await vi.advanceTimersByTimeAsync(10_000);
    expect(runs).toHaveLength(0);
  });

  it('invalidates the token and runs no trailing work when disposed mid-flight', async () => {
    const tokens: RefreshToken[] = [];
    let finish: (() => void) | null = null;
    const scheduler = createRefreshScheduler({
      name: 'test',
      delayMs: DELAY_MS,
      maxWaitMs: MAX_WAIT_MS,
      run: (token) => {
        tokens.push(token);
        return new Promise<void>((resolve) => { finish = resolve; });
      },
    });

    scheduler.request({ immediate: true });
    await flush();
    expect(tokens).toHaveLength(1);
    expect(tokens[0]!.isCurrent()).toBe(true);

    scheduler.request();
    scheduler.dispose();
    expect(tokens[0]!.isCurrent()).toBe(false);

    finish!();
    await vi.advanceTimersByTimeAsync(10_000);
    expect(tokens).toHaveLength(1);
  });

  it('arms ONE timer for a burst inside a single interval', async () => {
    const setSpy = vi.spyOn(globalThis, 'setTimeout');
    const clearSpy = vi.spyOn(globalThis, 'clearTimeout');
    const runs: number[] = [];
    const scheduler = createRefreshScheduler({
      name: 'test',
      delayMs: DELAY_MS,
      maxWaitMs: MAX_WAIT_MS,
      run: async () => { runs.push(Date.now()); },
    });

    for (let i = 0; i < 100; i += 1) scheduler.request();

    // The whole point: a hundred events cost one timer, not a hundred
    // clear/set pairs.
    expect(setSpy).toHaveBeenCalledTimes(1);
    expect(clearSpy).not.toHaveBeenCalled();

    await vi.advanceTimersByTimeAsync(DELAY_MS);
    expect(runs).toHaveLength(1);
    scheduler.dispose();
  });

  it('measures the minimum interval from the run\'s completion, not its start', async () => {
    const runs: number[] = [];
    const scheduler = createRefreshScheduler({
      name: 'test',
      delayMs: DELAY_MS,
      maxWaitMs: MAX_WAIT_MS,
      // A slow RPC: 500ms on the wire, five times the coalescing delay.
      run: async () => {
        runs.push(Date.now());
        await new Promise<void>((resolve) => { setTimeout(resolve, 500); });
      },
    });

    const startedAt = Date.now();
    scheduler.request({ immediate: true });
    await flush();
    scheduler.request();

    await vi.advanceTimersByTimeAsync(500);
    expect(runs).toHaveLength(1);

    // A cooldown measured from the START would have re-issued at +100, on top
    // of a call still in flight. It is measured from completion, so the next
    // run lands at 500 + 100.
    await vi.advanceTimersByTimeAsync(DELAY_MS - 1);
    expect(runs).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(runs).toHaveLength(2);
    expect(runs[1]! - startedAt).toBe(500 + DELAY_MS);
    scheduler.dispose();
  });

  it('starts an immediate request without waiting out the delay', async () => {
    const runs: number[] = [];
    const scheduler = createRefreshScheduler({
      name: 'test',
      delayMs: DELAY_MS,
      maxWaitMs: MAX_WAIT_MS,
      run: async () => { runs.push(Date.now()); },
    });

    const startedAt = Date.now();
    scheduler.request({ immediate: true });
    await flush();
    expect(runs).toEqual([startedAt]);
    scheduler.dispose();
  });
});
