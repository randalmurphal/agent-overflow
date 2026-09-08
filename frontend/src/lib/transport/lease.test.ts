import { afterEach, describe, expect, it, vi } from 'vitest';
import { __resetClientLeaseForTest, onClientLeaseChange, setClientLease } from './lease';
import { wsClient } from './wsClient';

afterEach(() => {
  __resetClientLeaseForTest();
  vi.restoreAllMocks();
});

describe('setClientLease', () => {
  it('states a CHANGE to every backend once, and an unchanged state to nobody', () => {
    const setLease = vi.spyOn(wsClient, 'setLease').mockImplementation(() => {});
    const seen: string[] = [];
    const off = onClientLeaseChange((state) => seen.push(state));
    setClientLease('background');
    setClientLease('background');
    setClientLease('active');
    // The dedup is here, before the fan-out: a repeated state reaches no
    // connection and no listener, not even to be dropped by each of them.
    expect(setLease.mock.calls).toEqual([['background'], ['active']]);
    expect(seen).toEqual(['active', 'background', 'active']);
    off();
  });

  it('the reset restates the resting state to every backend', () => {
    setClientLease('background');
    const setLease = vi.spyOn(wsClient, 'setLease').mockImplementation(() => {});
    __resetClientLeaseForTest();
    expect(setLease).toHaveBeenCalledWith('active');
  });
});
