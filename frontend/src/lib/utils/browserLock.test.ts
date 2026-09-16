import { beforeEach, describe, expect, it, vi } from 'vitest';
import { BROWSER_LOCK_KEY, createBrowserLock } from './browserLock';
import { DEFAULT_LOCK_WINDOW_MS } from './lockTiming';

beforeEach(() => localStorage.clear());

function setup(enabled = true) {
  if (enabled) localStorage.setItem(BROWSER_LOCK_KEY, 'enabled');
  let time = 1_000;
  const verify = vi.fn(async () => {});
  const changed = vi.fn();
  const storage = {
    getItem: (key: string) => localStorage.getItem(key),
    setItem: vi.fn((key: string, value: string) => localStorage.setItem(key, value)),
    removeItem: (key: string) => localStorage.removeItem(key),
  };
  const lock = createBrowserLock({ storage, verify, changed, now: () => time });
  return { lock, verify, changed, storage, advance: (ms: number) => { time += ms; } };
}

describe('browser lock lifecycle', () => {
  it('starts each tab locked and never stores an unlock', async () => {
    const first = setup();
    const second = setup();
    expect(first.lock.snapshot().locked).toBe(true);
    await first.lock.unlock();
    expect(first.lock.snapshot().locked).toBe(false);
    expect(second.lock.snapshot().locked).toBe(true);
    expect(localStorage.length).toBe(1);
    expect(setup().lock.snapshot().locked).toBe(true);
  });

  it('covers immediately, permits a short return, and requires proof after five minutes', async () => {
    const { lock, verify, advance } = setup();
    await lock.unlock();
    lock.visibilityChanged(true);
    expect(lock.snapshot().locked).toBe(true);
    advance(4_000);
    lock.visibilityChanged(false);
    expect(lock.snapshot().locked).toBe(false);
    expect(verify).toHaveBeenCalledTimes(1);
    lock.visibilityChanged(true);
    advance(DEFAULT_LOCK_WINDOW_MS);
    lock.visibilityChanged(false);
    expect(lock.snapshot().locked).toBe(true);
    await lock.unlock();
    expect(verify).toHaveBeenCalledTimes(2);
    expect(lock.snapshot().locked).toBe(false);
  });

  it('keeps a failed or canceled verification locked through a short background trip', async () => {
    const { lock, verify } = setup();
    verify.mockRejectedValue(new Error('Canceled'));
    await lock.unlock();
    lock.visibilityChanged(true);
    lock.visibilityChanged(false);
    expect(lock.snapshot()).toMatchObject({ locked: true, busy: false, error: expect.stringContaining('Canceled') });
    verify.mockResolvedValue(undefined);
    await lock.unlock();
    expect(lock.snapshot()).toMatchObject({ locked: false, error: '' });
  });

  it('verifies before enabling and retains the old preference on partial failure', async () => {
    const { lock, verify, storage } = setup(false);
    verify.mockRejectedValue(new Error('No passkey'));
    await lock.enable();
    expect(lock.snapshot().enabled).toBe(false);
    expect(localStorage.getItem(BROWSER_LOCK_KEY)).toBeNull();
    verify.mockResolvedValue(undefined);
    storage.setItem.mockImplementationOnce(() => { throw new Error('Storage full'); });
    await lock.enable();
    expect(lock.snapshot().enabled).toBe(false);
    expect(lock.snapshot().error).toContain('Storage full');
    await lock.enable();
    expect(lock.snapshot()).toMatchObject({ enabled: true, locked: false, error: '' });
  });

  it('can retry a verifier that fails before returning a promise', async () => {
    const { lock, verify } = setup();
    verify.mockImplementationOnce(() => { throw new Error('Unavailable'); });
    await lock.unlock();
    expect(lock.snapshot()).toMatchObject({ locked: true, busy: false });
    await lock.unlock();
    expect(lock.snapshot()).toMatchObject({ locked: false, busy: false });
    expect(verify).toHaveBeenCalledTimes(2);
  });

  it('disabling clears the saved preference and background state; enabling verifies again', async () => {
    const { lock, verify, advance } = setup();
    await lock.unlock();
    lock.disable();
    expect(localStorage.getItem(BROWSER_LOCK_KEY)).toBeNull();
    lock.visibilityChanged(true);
    advance(DEFAULT_LOCK_WINDOW_MS);
    lock.visibilityChanged(false);
    expect(lock.snapshot().locked).toBe(false);
    await lock.enable();
    expect(verify).toHaveBeenCalledTimes(2);
    lock.visibilityChanged(true);
    lock.visibilityChanged(false);
    expect(lock.snapshot().locked).toBe(false);
  });

  it('a preference change from another tab cannot dismiss this tab’s cover', async () => {
    const { lock } = setup();
    localStorage.removeItem(BROWSER_LOCK_KEY);
    lock.preferencesChanged();
    lock.disable();
    expect(lock.snapshot().locked).toBe(true);
    await lock.unlock();
    expect(lock.snapshot()).toMatchObject({ enabled: false, locked: false });
  });

  it('restoration invalidates an outstanding proof and repeated clicks share one request', async () => {
    const { lock, verify } = setup();
    let finish!: () => void;
    verify.mockImplementation(() => new Promise<void>((resolve) => { finish = resolve; }));
    const pending = lock.unlock();
    void lock.unlock();
    expect(verify).toHaveBeenCalledTimes(1);
    lock.pageRestored();
    finish();
    await pending;
    expect(lock.snapshot()).toMatchObject({ locked: true, busy: false });
    verify.mockResolvedValue(undefined);
    await lock.unlock();
    lock.pageRestored();
    expect(lock.snapshot().locked).toBe(true);
  });

  it('a proof finishing while hidden keeps the cover until a permitted return', async () => {
    const { lock, verify, advance } = setup();
    let finish!: () => void;
    verify.mockImplementation(() => new Promise<void>((resolve) => { finish = resolve; }));
    const pending = lock.unlock();
    lock.visibilityChanged(true);
    finish();
    await pending;
    expect(lock.snapshot().locked).toBe(true);
    advance(DEFAULT_LOCK_WINDOW_MS);
    lock.visibilityChanged(false);
    expect(lock.snapshot().locked).toBe(true);
  });

  it('host exemption is explicit and does not erase a paired browser’s preference', () => {
    const { lock, verify } = setup();
    lock.setLocalPage(true);
    expect(lock.snapshot().locked).toBe(false);
    lock.visibilityChanged(true);
    lock.visibilityChanged(false);
    expect(lock.snapshot().locked).toBe(false);
    expect(verify).not.toHaveBeenCalled();
    expect(localStorage.getItem(BROWSER_LOCK_KEY)).toBe('enabled');
    lock.setLocalPage(false);
    expect(lock.snapshot().locked).toBe(true);
  });

  it('storage read failure stays locked and disposal ignores late verification', async () => {
    const lock = createBrowserLock({
      storage: { ...localStorage, getItem: () => { throw new Error('Denied'); }, setItem: vi.fn(), removeItem: vi.fn() },
      verify: async () => {}, changed: vi.fn(),
    });
    expect(lock.snapshot()).toMatchObject({ enabled: true, locked: true, error: expect.stringContaining('Denied') });
    const { lock: pendingLock, verify, changed } = setup();
    let finish!: () => void;
    verify.mockImplementation(() => new Promise<void>((resolve) => { finish = resolve; }));
    const pending = pendingLock.unlock();
    pendingLock.dispose();
    const calls = changed.mock.calls.length;
    finish();
    await pending;
    expect(changed).toHaveBeenCalledTimes(calls);
    expect(pendingLock.snapshot().locked).toBe(true);
  });
});
