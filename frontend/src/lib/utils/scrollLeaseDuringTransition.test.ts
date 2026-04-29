import { afterEach, describe, expect, it, vi } from 'vitest';
import { leaseDuringSettle } from './scrollLeaseDuringTransition';

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

function makeController(): { pauseAutoScroll: () => () => void; release: ReturnType<typeof vi.fn>; pauseCalls: number } {
  const release = vi.fn();
  const ctrl = {
    pauseCalls: 0,
    release,
    pauseAutoScroll: vi.fn(() => {
      ctrl.pauseCalls += 1;
      return release;
    }),
  };
  return ctrl;
}

describe('leaseDuringSettle', () => {
  it('is a no-op when no controller is provided', () => {
    expect(() => leaseDuringSettle(undefined)).not.toThrow();
    expect(() => leaseDuringSettle(null)).not.toThrow();
  });

  it('acquires the lease and releases after two animation frames by default', async () => {
    const ctrl = makeController();
    leaseDuringSettle(ctrl);
    expect(ctrl.pauseCalls).toBe(1);
    expect(ctrl.release).not.toHaveBeenCalled();
    // Wait two animation frames.
    await new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve())));
    // The release runs in the inner rAF, so the resolve race may settle
    // a tick before we observe; flush a microtask.
    await Promise.resolve();
    expect(ctrl.release).toHaveBeenCalledTimes(1);
  });

  it('releases after the supplied duration when transitionMs is given', () => {
    vi.useFakeTimers();
    const ctrl = makeController();
    leaseDuringSettle(ctrl, 250);
    expect(ctrl.pauseCalls).toBe(1);
    expect(ctrl.release).not.toHaveBeenCalled();
    vi.advanceTimersByTime(249);
    expect(ctrl.release).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(ctrl.release).toHaveBeenCalledTimes(1);
  });

  it('returned dispose function releases early and is idempotent', () => {
    vi.useFakeTimers();
    const ctrl = makeController();
    const release = leaseDuringSettle(ctrl, 250);
    release();
    expect(ctrl.release).toHaveBeenCalledTimes(1);
    release();
    // Second call is a no-op.
    expect(ctrl.release).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(300);
    // The setTimeout callback also runs, but the dispose flag suppresses
    // a second release.
    expect(ctrl.release).toHaveBeenCalledTimes(1);
  });
});
