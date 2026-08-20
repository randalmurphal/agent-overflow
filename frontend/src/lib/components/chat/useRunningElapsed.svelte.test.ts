import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import { render } from '@testing-library/svelte';
import UseRunningElapsedHarness from './UseRunningElapsedHarness.svelte';
import {
  __resetRunningElapsedTickerForTest,
  __runningElapsedAcquireCountForTest,
  __runningElapsedTickerActiveForTest,
  __runningElapsedTickerSubscribersForTest,
} from './useRunningElapsed.svelte';

describe('createRunningElapsed', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(5_000);
    __resetRunningElapsedTickerForTest();
  });

  afterEach(() => {
    __resetRunningElapsedTickerForTest();
    vi.useRealTimers();
  });

  it('shares one interval across multiple running labels', async () => {
    const setIntervalSpy = vi.spyOn(globalThis, 'setInterval');
    try {
      const r = render(UseRunningElapsedHarness, {
        props: {
          firstRunning: true,
          secondRunning: true,
          createdAt: 1_000,
        },
      });

      await tick();
      expect(r.getByTestId('subscriber-count').textContent).toBe('2');

      expect(setIntervalSpy).toHaveBeenCalledTimes(1);
      expect(__runningElapsedTickerActiveForTest()).toBe(true);

      await r.rerender({
        firstRunning: false,
        secondRunning: true,
        createdAt: 1_000,
      });

      await tick();
      expect(r.getByTestId('subscriber-count').textContent).toBe('1');
      expect(setIntervalSpy).toHaveBeenCalledTimes(1);
      expect(__runningElapsedTickerActiveForTest()).toBe(true);

      r.unmount();
      await tick();
      expect(__runningElapsedTickerSubscribersForTest()).toBe(0);
      expect(__runningElapsedTickerActiveForTest()).toBe(false);
    } finally {
      setIntervalSpy.mockRestore();
    }
  });

  it('does not release and re-acquire the ticker when the tracked object is replaced with an equal status', async () => {
    // The defect class (2026-08-19 sweep): production `isTicking` closures
    // read `item.status` off an object the streaming pipeline re-derives
    // FRESH per delta. Without the factory's `ticking` $derived cutoff the
    // acquire effect subscribes to the object itself, so every delta
    // released and re-acquired the shared ticker — whose acquire writes the
    // global `sharedNow`, waking every elapsed label in the app.
    const r = render(UseRunningElapsedHarness, {
      props: {
        firstRunning: false,
        secondRunning: false,
        churnItem: { status: 'running' },
      },
    });

    await tick();
    expect(__runningElapsedTickerSubscribersForTest()).toBe(1);
    expect(__runningElapsedAcquireCountForTest()).toBe(1);

    // The streaming delta: fresh object, same status.
    await r.rerender({ churnItem: { status: 'running' } });
    await tick();
    expect(__runningElapsedAcquireCountForTest()).toBe(1);
    expect(__runningElapsedTickerSubscribersForTest()).toBe(1);

    // A real status flip still releases.
    await r.rerender({ churnItem: { status: 'completed' } });
    await tick();
    expect(__runningElapsedTickerSubscribersForTest()).toBe(0);
  });

  it('updates each running label from the shared clock', async () => {
    const r = render(UseRunningElapsedHarness, {
      props: {
        firstRunning: true,
        secondRunning: true,
        createdAt: 1_000,
      },
    });

    await tick();
    expect(r.getByTestId('first-label').textContent).toBe('4s');
    expect(r.getByTestId('second-label').textContent).toBe('4s');

    vi.advanceTimersByTime(1_000);

    await tick();
    expect(r.getByTestId('first-label').textContent).toBe('5s');
    expect(r.getByTestId('second-label').textContent).toBe('5s');
  });
});
