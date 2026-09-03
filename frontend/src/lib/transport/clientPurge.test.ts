// The purge seam: who registers, what a scope means, and the promise that
// a step which fails cannot strand the person doing the removing.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  __resetClientPurgeForTest,
  onPurgeClientState,
  purgeClientState,
  type PurgeScope,
} from './clientPurge';

beforeEach(() => {
  __resetClientPurgeForTest();
});

afterEach(() => {
  __resetClientPurgeForTest();
  vi.restoreAllMocks();
});

describe('purgeClientState', () => {
  it('hands every registered step the scope it was called with', () => {
    const seen: PurgeScope[] = [];
    onPurgeClientState((scope) => void seen.push(scope));
    onPurgeClientState((scope) => void seen.push(scope));

    purgeClientState('remote-machine');
    expect(seen).toEqual(['remote-machine', 'remote-machine']);
  });

  // The home backend's registry id is the empty string, so "drop home" and
  // "drop everything" are two different values that must never collapse.
  it('keeps null (every backend) distinct from the empty string (home)', () => {
    const seen: PurgeScope[] = [];
    onPurgeClientState((scope) => void seen.push(scope));

    purgeClientState(null);
    purgeClientState('');
    expect(seen).toEqual([null, '']);
  });

  it('runs the remaining steps when one throws, and does not rethrow', () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const after = vi.fn();
    onPurgeClientState(() => {
      throw new Error('storage is gone');
    });
    onPurgeClientState(after);

    expect(() => purgeClientState(null)).not.toThrow();
    expect(after).toHaveBeenCalledWith(null);
  });

  it('contains a rejected async step rather than leaving it unhandled', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    onPurgeClientState(() => Promise.reject(new Error('delete blocked')));

    purgeClientState(null);
    await Promise.resolve();
    await Promise.resolve();
    expect(warn).toHaveBeenCalled();
  });

  it('stops calling a step once its remover runs', () => {
    const step = vi.fn();
    const remove = onPurgeClientState(step);

    purgeClientState(null);
    remove();
    purgeClientState(null);
    expect(step).toHaveBeenCalledTimes(1);
  });
});
