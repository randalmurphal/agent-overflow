import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { addErrorToast, addToast, getToasts, holdToast, releaseToast, removeToast } from './toast.svelte';

describe('toast store', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    // Drain any state from prior tests.
    for (const t of [...getToasts()]) removeToast(t.id);
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('appends a new toast with monotonically unique ids', () => {
    const id1 = addToast('info', 'first');
    const id2 = addToast('error', 'second');
    expect(id1).not.toBe(id2);
    const list = getToasts();
    expect(list.map((t) => t.id)).toEqual([id1, id2]);
    expect(list.map((t) => t.message)).toEqual(['first', 'second']);
  });

  it('removes a toast automatically after its duration elapses', () => {
    const id = addToast('info', 'expires', 1000);
    expect(getToasts()).toHaveLength(1);
    vi.advanceTimersByTime(999);
    expect(getToasts()).toHaveLength(1);
    vi.advanceTimersByTime(2);
    expect(getToasts().find((t) => t.id === id)).toBeUndefined();
  });

  it('removeToast cancels the pending timer', () => {
    const id = addToast('info', 'cancel me', 1000);
    removeToast(id);
    expect(getToasts().find((t) => t.id === id)).toBeUndefined();

    // Fast-forward; nothing else should happen / throw.
    vi.advanceTimersByTime(5000);
    expect(getToasts().find((t) => t.id === id)).toBeUndefined();
  });

  it('removeToast on unknown id is a no-op', () => {
    const id = addToast('info', 'keep');
    removeToast('not-a-real-id');
    expect(getToasts().find((t) => t.id === id)).toBeDefined();
  });

  it('retains an actionable failure until explicitly dismissed', () => {
    const run = vi.fn();
    const id = addToast('error', 'Computer unavailable', 0, { label: 'Try again', run });
    vi.advanceTimersByTime(60_000);
    expect(getToasts().find((toast) => toast.id === id)?.action?.label).toBe('Try again');
    expect(run).not.toHaveBeenCalled();
    removeToast(id);
    expect(getToasts()).toEqual([]);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('keeps the error behind an error toast', () => {
    const err = new Error('disk full');
    const id = addErrorToast('Could not save: disk full', err);
    const toast = getToasts().find((t) => t.id === id);
    expect(toast?.type).toBe('error');
    expect(toast?.message).toBe('Could not save: disk full');
    expect(toast?.captured?.report.context).toBe('Could not save: disk full');
    expect(toast?.captured?.report.chain).toEqual(['disk full']);
    expect(addToast('error', 'plain') && getToasts().at(-1)?.captured).toBeUndefined();
  });

  it('holds a toast past its delay and restarts the full delay on release', () => {
    const id = addToast('info', 'held', 1000);
    vi.advanceTimersByTime(900);
    holdToast(id);
    vi.advanceTimersByTime(5000);
    expect(getToasts().find((t) => t.id === id)).toBeDefined();

    releaseToast(id);
    vi.advanceTimersByTime(999);
    expect(getToasts().find((t) => t.id === id)).toBeDefined();
    vi.advanceTimersByTime(2);
    expect(getToasts().find((t) => t.id === id)).toBeUndefined();
  });

  it('does not start a timer for a persistent toast on release', () => {
    const id = addToast('error', 'Computer unavailable', 0);
    holdToast(id);
    releaseToast(id);
    expect(vi.getTimerCount()).toBe(0);
    removeToast(id);
  });
});
