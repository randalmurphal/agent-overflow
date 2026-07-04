import { beforeEach, describe, expect, it } from 'vitest';
import {
  cycleUsagePeriod,
  getUsagePeriod,
  periodFromMillis,
  readPersistedPeriod,
  resetUsagePeriodForTest,
  setUsagePeriod,
} from './usagePeriod.svelte';

const STORAGE_KEY = 'agent-overflow:usage:period';

describe('usage period store', () => {
  beforeEach(() => {
    resetUsagePeriodForTest();
  });

  it('defaults to 30d when nothing is persisted', () => {
    expect(getUsagePeriod()).toBe('30d');
  });

  it('setUsagePeriod updates state and persists', () => {
    setUsagePeriod('1w');
    expect(getUsagePeriod()).toBe('1w');
    expect(localStorage.getItem(STORAGE_KEY)).toBe('1w');
  });

  describe('cycleUsagePeriod', () => {
    it('rotates 1d -> 1w -> 30d -> all -> 1d', () => {
      setUsagePeriod('1d');
      cycleUsagePeriod();
      expect(getUsagePeriod()).toBe('1w');
      cycleUsagePeriod();
      expect(getUsagePeriod()).toBe('30d');
      cycleUsagePeriod();
      expect(getUsagePeriod()).toBe('all');
      cycleUsagePeriod();
      expect(getUsagePeriod()).toBe('1d');
    });

    it('persists each step of the cycle', () => {
      setUsagePeriod('all');
      cycleUsagePeriod();
      expect(localStorage.getItem(STORAGE_KEY)).toBe('1d');
    });
  });

  describe('readPersistedPeriod', () => {
    it('returns default when no value is stored', () => {
      expect(readPersistedPeriod()).toBe('30d');
    });

    it('returns default when the stored value is garbage', () => {
      localStorage.setItem(STORAGE_KEY, 'garbage');
      expect(readPersistedPeriod()).toBe('30d');
    });

    it('returns default when the stored value is empty', () => {
      localStorage.setItem(STORAGE_KEY, '');
      expect(readPersistedPeriod()).toBe('30d');
    });

    it('round-trips a valid stored value', () => {
      localStorage.setItem(STORAGE_KEY, '1w');
      expect(readPersistedPeriod()).toBe('1w');
    });
  });

  describe('periodFromMillis', () => {
    const now = Date.UTC(2026, 6, 3, 12, 0, 0);
    const DAY_MS = 24 * 60 * 60 * 1000;

    it('"all" is unbounded (0)', () => {
      expect(periodFromMillis('all', now)).toBe(0);
    });

    it('"1d" is now minus 1 day', () => {
      expect(periodFromMillis('1d', now)).toBe(now - DAY_MS);
    });

    it('"1w" is now minus 7 days', () => {
      expect(periodFromMillis('1w', now)).toBe(now - 7 * DAY_MS);
    });

    it('"30d" is now minus 30 days', () => {
      expect(periodFromMillis('30d', now)).toBe(now - 30 * DAY_MS);
    });
  });
});
