import { describe, expect, it, vi } from 'vitest';
import { retryingLoad } from './retryingLoad';

describe('retryingLoad', () => {
  it('runs the load once for every call while it is out and after it succeeded', async () => {
    const load = vi.fn(async () => 'module');
    const get = retryingLoad(load);
    const [a, b] = [get(), get()];
    expect(a).toBe(b);
    expect(await a).toBe('module');
    expect(await get()).toBe('module');
    expect(load).toHaveBeenCalledTimes(1);
  });

  it('forgets a load that failed, so the next call loads again', async () => {
    const load = vi.fn()
      .mockRejectedValueOnce(new Error('the chunk did not load'))
      .mockResolvedValueOnce('module');
    const get = retryingLoad(load);
    await expect(get()).rejects.toThrow('the chunk did not load');
    expect(await get()).toBe('module');
    expect(load).toHaveBeenCalledTimes(2);
  });
});
