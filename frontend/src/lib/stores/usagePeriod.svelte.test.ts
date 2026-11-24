import { beforeEach, describe, expect, it } from 'vitest';
import {
  cycleUsagePeriod,
  getUsagePeriod,
  periodFromMillis,
  readPersistedPeriod,
  setUsagePeriod,
  syncUsagePeriodFromSettings,
} from './usagePeriod.svelte';
import { loadSettings, resetSettingsForTest } from './settings.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';

const STORAGE_KEY = 'agent-overflow:usage:period';

describe('usage period store', () => {
  beforeEach(() => {
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

  it('setUsagePeriod writes through to Go settings (localStorage is not durable)', async () => {
    // The webview origin changes every launch (ephemeral transport
    // port), so localStorage alone silently resets the period; the Go
    // settings patch is the durable copy. A device preference reaches the
    // computer once its settings answer names the key.
    setBindingMock('GetSettings', async () => ({ usagePeriod: 'month' }));
    const updateMock = setBindingMock('UpdateSettings', async () => null);
    setUsagePeriod('day');
    expect(updateMock).not.toHaveBeenCalled();
    await loadSettings();
    expect(updateMock).toHaveBeenCalledWith(
      expect.objectContaining({ usagePeriod: 'day' }),
    );
    expect(getUsagePeriod()).toBe('day');
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

    // A period persisted before the calendar-alignment change lands on the
    // closest calendar period, not the default.
    it.each([
      ['returns default when the stored value is garbage', 'garbage', 'month'],
      ['returns default when the stored value is empty', '', 'month'],
      ['round-trips a valid stored value', 'week', 'week'],
      ['migrates the pre-calendar rolling-window value 1d', '1d', 'day'],
      ['migrates the pre-calendar rolling-window value 1w', '1w', 'week'],
      ['migrates the pre-calendar rolling-window value 30d', '30d', 'month'],
    ])('%s', (_name, stored, expected) => {
      localStorage.setItem(STORAGE_KEY, stored);
      expect(readPersistedPeriod()).toBe(expected);
    });
  });

  describe('periodFromMillis', () => {
    // Friday 2026-07-03 15:30 LOCAL time — calendar boundaries are
    // local-midnight based (the backend query carries the matching
    // tzOffsetMinutes), so fixtures use the local Date constructor.
    const now = new Date(2026, 6, 3, 15, 30, 0).getTime();

    // 2026-07-03 is a Friday; its week started Sunday 2026-06-28.
    it.each([
      ['"all" is unbounded (0)', 'all', now, 0],
      ['"day" is local midnight of today', 'day', now, new Date(2026, 6, 3).getTime()],
      ['"week" is local midnight of the most recent Sunday', 'week', now, new Date(2026, 5, 28).getTime()],
      ['"week" on a Sunday is that Sunday\'s own midnight', 'week',
        new Date(2026, 5, 28, 14, 0, 0).getTime(), new Date(2026, 5, 28).getTime()],
      ['"month" is local midnight of the 1st', 'month', now, new Date(2026, 6, 1).getTime()],
      ['"month" on the 1st is that day\'s own midnight', 'month',
        new Date(2026, 6, 1, 9, 0, 0).getTime(), new Date(2026, 6, 1).getTime()],
    ] as const)('%s', (_name, period, instant, start) => {
      expect(periodFromMillis(period, instant)).toBe(start);
    });
  });
});
