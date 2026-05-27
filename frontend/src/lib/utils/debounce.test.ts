import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { debounce } from './debounce';

describe('debounce', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it('fires exactly once after the trailing window even when called in a burst', () => {
    const fn = vi.fn();
    const d = debounce(fn, 50);

    d();
    d();
    d();
    expect(fn).not.toHaveBeenCalled();

    vi.advanceTimersByTime(49);
    expect(fn).not.toHaveBeenCalled();

    vi.advanceTimersByTime(1);
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it('resets the timer on each call so the window always measures from the last invocation', () => {
    const fn = vi.fn();
    const d = debounce(fn, 100);

    d();
    vi.advanceTimersByTime(80);
    d();
    vi.advanceTimersByTime(80);
    // 80+80 = 160 ms elapsed total, but only 80 ms since the last call:
    // the handler must not have fired yet.
    expect(fn).not.toHaveBeenCalled();

    vi.advanceTimersByTime(20);
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it('passes the last-arriving args to the handler', () => {
    const fn = vi.fn();
    const d = debounce<[number]>(fn, 50);
    d(1);
    d(2);
    d(3);
    vi.advanceTimersByTime(50);
    expect(fn).toHaveBeenCalledWith(3);
  });

  it('cancel() suppresses a pending invocation', () => {
    const fn = vi.fn();
    const d = debounce(fn, 50);
    d();
    d.cancel();
    vi.advanceTimersByTime(50);
    expect(fn).not.toHaveBeenCalled();
  });

  it('flush() runs the pending invocation immediately with the latest args', () => {
    const fn = vi.fn();
    const d = debounce<[number]>(fn, 50);

    d(1);
    d(2);
    expect(d.flush()).toBe(true);

    expect(fn).toHaveBeenCalledTimes(1);
    expect(fn).toHaveBeenCalledWith(2);
    vi.advanceTimersByTime(50);
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it('flush() is safe to call when nothing is pending', () => {
    const fn = vi.fn();
    const d = debounce(fn, 50);

    expect(d.flush()).toBe(false);
    expect(fn).not.toHaveBeenCalled();
  });

  it('cancel() is safe to call when nothing is pending', () => {
    const fn = vi.fn();
    const d = debounce(fn, 50);
    // Never invoked — cancel must no-op.
    expect(() => d.cancel()).not.toThrow();
    // After a fire + cancel, a subsequent cancel is also a no-op.
    d();
    vi.advanceTimersByTime(50);
    expect(fn).toHaveBeenCalledTimes(1);
    expect(() => d.cancel()).not.toThrow();
    expect(() => d.cancel()).not.toThrow();
  });

  it('a new invocation after the handler fired restarts the cycle cleanly', () => {
    const fn = vi.fn();
    const d = debounce(fn, 50);
    d();
    vi.advanceTimersByTime(50);
    expect(fn).toHaveBeenCalledTimes(1);

    d();
    vi.advanceTimersByTime(50);
    expect(fn).toHaveBeenCalledTimes(2);
  });
});
