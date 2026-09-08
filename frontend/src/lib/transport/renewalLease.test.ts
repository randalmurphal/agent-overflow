import { afterEach, describe, expect, it, vi } from 'vitest';
import { withRenewalLease } from './renewalLease';

type LockRequest = (name: string, options: { signal?: AbortSignal }, callback: () => Promise<unknown>) => Promise<unknown>;

// happy-dom has no Web Locks, so the fallback lease is what runs unless a
// `navigator.locks` is installed. The fake below IS the Web Locks branch's
// only test double: it decides whether the grant arrives.
function installLocks(request: LockRequest) {
  const fn = vi.fn(request);
  Object.defineProperty(navigator, 'locks', { value: { request: fn }, configurable: true });
  return fn;
}

afterEach(() => {
  delete (navigator as { locks?: unknown }).locks;
});

describe('withRenewalLease under Web Locks', () => {
  it('bounds the wait for the grant, and an aborted wait runs the work unheld', async () => {
    let options: { signal?: AbortSignal } | undefined;
    const request = installLocks(async (_name, opts) => {
      options = opts;
      // The holder never finishes: the bounded signal fires before the
      // grant, and the request rejects without ever running the callback.
      throw new DOMException('The lock request is aborted', 'AbortError');
    });
    const seen: boolean[] = [];
    const result = await withRenewalLease('k', async (hold) => {
      seen.push(hold.held());
      return 'ran';
    });
    expect(result).toBe('ran');
    expect(seen).toEqual([false]);
    expect(request).toHaveBeenCalledTimes(1);
    // The wait is bounded by a timeout signal that was live when the
    // request was made, never a pre-aborted one.
    expect(options?.signal).toBeInstanceOf(AbortSignal);
    expect(options?.signal?.aborted).toBe(false);
  });

  it('runs the work held once the lock is granted and answers its result', async () => {
    installLocks(async (_name, _options, callback) => callback());
    const seen: boolean[] = [];
    const result = await withRenewalLease('k', async (hold) => {
      seen.push(hold.held());
      return 42;
    });
    expect(result).toBe(42);
    expect(seen).toEqual([true]);
  });

  it("propagates the work's own rejection after the grant rather than rerunning it", async () => {
    installLocks(async (_name, _options, callback) => callback());
    const work = vi.fn(async () => {
      throw new Error('exchange failed');
    });
    await expect(withRenewalLease('k', work)).rejects.toThrow('exchange failed');
    expect(work).toHaveBeenCalledTimes(1);
  });
});
