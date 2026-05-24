import { afterEach, describe, expect, it } from 'vitest';
import { flushSync } from 'svelte';

import {
  getSystemStats,
  resetForTest,
  setSystemStats,
} from './systemStats.svelte';

describe('systemStats store', () => {
  afterEach(() => {
    resetForTest();
  });

  it('returns null before the first event arrives', () => {
    expect(getSystemStats()).toBeNull();
  });

  it('round-trips set/get with the latest snapshot', () => {
    setSystemStats({
      isWsl: true,
      cpuPercent: 42.5,
      memUsedBytes: 4 * 1024 ** 3,
      memTotalBytes: 16 * 1024 ** 3,
    });

    const stats = getSystemStats();
    expect(stats?.isWsl).toBe(true);
    expect(stats?.cpuPercent).toBe(42.5);
    expect(stats?.memUsedBytes).toBe(4 * 1024 ** 3);
    expect(stats?.memTotalBytes).toBe(16 * 1024 ** 3);
  });

  it('overwrites the previous snapshot on every set', () => {
    setSystemStats({ isWsl: true, cpuPercent: 10, memUsedBytes: 1, memTotalBytes: 2 });
    setSystemStats({ isWsl: false, cpuPercent: 80, memUsedBytes: 3, memTotalBytes: 4 });

    expect(getSystemStats()).toEqual({
      isWsl: false,
      cpuPercent: 80,
      memUsedBytes: 3,
      memTotalBytes: 4,
    });
  });

  it('resetForTest clears the snapshot back to null', () => {
    setSystemStats({ isWsl: false, cpuPercent: 1, memUsedBytes: 2, memTotalBytes: 3 });
    resetForTest();
    expect(getSystemStats()).toBeNull();
  });

  it('triggers $derived consumers when a snapshot is set after construction', () => {
    // Same load-bearing assertion accountInfo.svelte.test.ts encodes:
    // the SystemStatsFooter's `$derived(getSystemStats())` depends on
    // setSystemStats invalidating derived reads. Without this, the
    // sidebar would mount before the first event and stay null
    // forever.
    const reads: Array<number | undefined> = [];

    const stop = $effect.root(() => {
      const cpu = $derived(getSystemStats()?.cpuPercent);
      $effect(() => {
        reads.push(cpu);
      });
    });

    try {
      flushSync();
      expect(reads.at(-1)).toBeUndefined();

      setSystemStats({ isWsl: false, cpuPercent: 25, memUsedBytes: 1, memTotalBytes: 2 });
      flushSync();
      expect(reads.at(-1)).toBe(25);

      setSystemStats({ isWsl: false, cpuPercent: 75, memUsedBytes: 1, memTotalBytes: 2 });
      flushSync();
      expect(reads.at(-1)).toBe(75);
    } finally {
      stop();
    }
  });
});
