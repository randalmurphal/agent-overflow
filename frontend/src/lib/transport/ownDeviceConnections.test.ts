import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ownDeviceConnectionExcluded, ownDeviceConnectionPolicy, rememberOwnDeviceMemberships, setOwnDeviceConnectionExcluded } from './ownDeviceConnections';

const KEY = 'agent-overflow:own-device-membership';
const member = (backendId: string, generation = 1, removed = false) => ({ backendId, keyThumbprint: backendId, generation, removed });
beforeEach(() => localStorage.clear());
afterEach(() => vi.restoreAllMocks());

describe('durable own-device admission hints', () => {
  it('preserves removals against stale catalogs while explicit rejoin and local exclusion remain independent', () => {
    rememberOwnDeviceMemberships([member('a', 2, true)]);
    rememberOwnDeviceMemberships([member('a'), member('a', 2)]);
    expect(ownDeviceConnectionExcluded('a')).toBe(true);
    setOwnDeviceConnectionExcluded('a', true);
    rememberOwnDeviceMemberships([member('a', 3)]);
    expect(ownDeviceConnectionPolicy().removed('a')).toBe(false);
    expect(ownDeviceConnectionExcluded('a')).toBe(true);
    setOwnDeviceConnectionExcluded('a', false);
    expect(ownDeviceConnectionExcluded('a')).toBe(false);
  });

  it('rejects an invalid catalog atomically and refuses damaged or conflicting stored identities', () => {
    rememberOwnDeviceMemberships([member('a', 2, true)]);
    const before = localStorage.getItem(KEY);
    expect(() => rememberOwnDeviceMemberships([member('b'), member('c', NaN)])).toThrow(/invalid/);
    expect(localStorage.getItem(KEY)).toBe(before);
    expect(() => rememberOwnDeviceMemberships([{ ...member('other', 3), keyThumbprint: 'a' }])).toThrow(/conflicting/);
    localStorage.setItem(KEY, JSON.stringify([member('a'), member('a', 2, true)]));
    expect(() => ownDeviceConnectionExcluded('a')).toThrow(/duplicate/);
  });

  it('retains an old key tombstone while allowing an explicitly enrolled replacement for the same computer', () => {
    rememberOwnDeviceMemberships([member('a', 2, true), { ...member('a'), keyThumbprint: 'replacement' }]);
    rememberOwnDeviceMemberships([member('a')]);
    expect(ownDeviceConnectionExcluded('a')).toBe(false);
    expect(() => rememberOwnDeviceMemberships([member('a', 3)])).toThrow(/conflicting/);
    expect(ownDeviceConnectionExcluded('a')).toBe(false);
  });

  it('never evicts removal knowledge to make room for a new group member', () => {
    rememberOwnDeviceMemberships(Array.from({ length: 128 }, (_, i) => member(String(i), 1, true)));
    const before = localStorage.getItem(KEY);
    expect(() => rememberOwnDeviceMemberships([member('new')])).toThrow(/128-device/);
    expect(localStorage.getItem(KEY)).toBe(before);
    expect(ownDeviceConnectionExcluded('0')).toBe(true);
  });

  it('surfaces persistence failure before claiming a local exclusion was saved', () => {
    vi.spyOn(localStorage, 'setItem').mockImplementation(() => { throw new Error('quota'); });
    expect(() => setOwnDeviceConnectionExcluded('a', true)).toThrow('quota');
    expect(ownDeviceConnectionExcluded('a')).toBe(false);
  });
});
