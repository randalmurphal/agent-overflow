import { beforeEach, describe, expect, it } from 'vitest';
import {
  cycleUsagePeriod,
  getUsagePeriod,
  periodFromMillis,
  readPersistedPeriod,
  resetUsagePeriodForTest,
  setUsagePeriod,
  syncUsagePeriodFromSettings,
} from './usagePeriod.svelte';
import { loadSettings, resetSettingsForTest } from './settings.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';

const STORAGE_KEY = 'agent-overflow:usage:period';

describe('usage period store', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetSettingsForTest();
    resetUsagePeriodForTest();
    // setUsagePeriod writes through to Go settings; default the RPC to
    // a no-op so tests that aren't about persistence stay quiet.
    setBindingMock('UpdateSettings', async () => null);
  });

  it('defaults to month when nothing is persisted', () => {
    expect(getUsagePeriod()).toBe('month');
  });

  it('setUsagePeriod updates state and persists', () => {
    setUsagePeriod('week');
    expect(getUsagePeriod()).toBe('week');
    expect(localStorage.getItem(STORAGE_KEY)).toBe('week');
  });

  it('setUsagePeriod writes through to Go settings (localStorage is not durable)', () => {
    // The webview origin changes every launch (ephemeral transport
    // port), so localStorage alone silently resets the period; the Go
    // settings patch is the durable copy.
    const updateMock = setBindingMock('UpdateSettings', async () => null);
    setUsagePeriod('day');
    expect(updateMock).toHaveBeenCalledWith(
      expect.objectContaining({ usagePeriod: 'day' }),
    );
  });

  describe('syncUsagePeriodFromSettings', () => {
    it('adopts the Go-persisted value after loadSettings', async () => {
      setBindingMock('GetSettings', async () => ({ usagePeriod: 'day' }));
      await loadSettings();
      syncUsagePeriodFromSettings();
      expect(getUsagePeriod()).toBe('day');
      expect(localStorage.getItem(STORAGE_KEY)).toBe('day');
    });

    it('falls back to the default when Go holds garbage', async () => {
      setBindingMock('GetSettings', async () => ({ usagePeriod: 'bogus' }));
      await loadSettings();
      syncUsagePeriodFromSettings();
      expect(getUsagePeriod()).toBe('month');
    });

    it('pushes a non-default local selection up when Go still has the factory default', () => {
      // Precondition (first launch after this field moved into Go
      // settings): the local period carries a real selection while Go
      // still holds the factory default. Migration must push local →
      // Go instead of clobbering the user's choice.
      setUsagePeriod('week');
      resetSettingsForTest(); // Go side back at the factory default

      const updateMock = setBindingMock('UpdateSettings', async () => null);
      syncUsagePeriodFromSettings();
      expect(updateMock).not.toHaveBeenCalled();
      expect(getUsagePeriod()).toBe('week');
    });
  });

  describe('cycleUsagePeriod', () => {
    it('rotates day -> week -> month -> all -> day', () => {
      setUsagePeriod('day');
      cycleUsagePeriod();
      expect(getUsagePeriod()).toBe('week');
      cycleUsagePeriod();
      expect(getUsagePeriod()).toBe('month');
      cycleUsagePeriod();
      expect(getUsagePeriod()).toBe('all');
      cycleUsagePeriod();
      expect(getUsagePeriod()).toBe('day');
    });

    it('persists each step of the cycle', () => {
      setUsagePeriod('all');
      cycleUsagePeriod();
      expect(localStorage.getItem(STORAGE_KEY)).toBe('day');
    });
  });

  describe('readPersistedPeriod', () => {
    it('returns default when no value is stored', () => {
      expect(readPersistedPeriod()).toBe('month');
    });

    it('returns default when the stored value is garbage', () => {
      localStorage.setItem(STORAGE_KEY, 'garbage');
      expect(readPersistedPeriod()).toBe('month');
    });

    it('returns default when the stored value is empty', () => {
      localStorage.setItem(STORAGE_KEY, '');
      expect(readPersistedPeriod()).toBe('month');
    });

    it('round-trips a valid stored value', () => {
      localStorage.setItem(STORAGE_KEY, 'week');
      expect(readPersistedPeriod()).toBe('week');
    });

    it('migrates the pre-calendar rolling-window values', () => {
      // Users who persisted a period before the calendar-alignment
      // change must land on the closest calendar period, not the
      // default.
      localStorage.setItem(STORAGE_KEY, '1d');
      expect(readPersistedPeriod()).toBe('day');
      localStorage.setItem(STORAGE_KEY, '1w');
      expect(readPersistedPeriod()).toBe('week');
      localStorage.setItem(STORAGE_KEY, '30d');
      expect(readPersistedPeriod()).toBe('month');
    });
  });

  describe('periodFromMillis', () => {
    // Friday 2026-07-03 15:30 LOCAL time — calendar boundaries are
    // local-midnight based (the backend query carries the matching
    // tzOffsetMinutes), so fixtures use the local Date constructor.
    const now = new Date(2026, 6, 3, 15, 30, 0).getTime();

    it('"all" is unbounded (0)', () => {
      expect(periodFromMillis('all', now)).toBe(0);
    });

    it('"day" is local midnight of today', () => {
      expect(periodFromMillis('day', now)).toBe(new Date(2026, 6, 3).getTime());
    });

    it('"week" is local midnight of the most recent Sunday', () => {
      // 2026-07-03 is a Friday; the week started Sunday 2026-06-28.
      expect(periodFromMillis('week', now)).toBe(new Date(2026, 5, 28).getTime());
    });

    it('"week" on a Sunday is that Sunday\'s own midnight', () => {
      const sundayAfternoon = new Date(2026, 5, 28, 14, 0, 0).getTime();
      expect(periodFromMillis('week', sundayAfternoon)).toBe(new Date(2026, 5, 28).getTime());
    });

    it('"month" is local midnight of the 1st', () => {
      expect(periodFromMillis('month', now)).toBe(new Date(2026, 6, 1).getTime());
    });

    it('"month" on the 1st is that day\'s own midnight', () => {
      const firstMorning = new Date(2026, 6, 1, 9, 0, 0).getTime();
      expect(periodFromMillis('month', firstMorning)).toBe(new Date(2026, 6, 1).getTime());
    });
  });
});
