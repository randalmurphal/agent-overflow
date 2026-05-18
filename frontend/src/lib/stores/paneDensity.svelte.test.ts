import { beforeEach, describe, expect, it } from 'vitest';
import {
  getMinPaneWidth,
  getPaneDensityMode,
  PANE_DENSITY_STORAGE_KEY,
  readPersistedPaneDensity,
  resetPaneDensityForTest,
  setPaneDensityMode,
} from './paneDensity.svelte';

describe('paneDensity store', () => {
  beforeEach(() => {
    resetPaneDensityForTest();
  });

  it('defaults to compact', () => {
    expect(getPaneDensityMode()).toBe('compact');
    expect(getMinPaneWidth()).toBe(560);
  });

  it('persists selected density in localStorage', () => {
    setPaneDensityMode('spacious');

    expect(getPaneDensityMode()).toBe('spacious');
    expect(getMinPaneWidth()).toBe(1400);
    expect(localStorage.getItem(PANE_DENSITY_STORAGE_KEY)).toBe('spacious');
  });

  it('ignores corrupt persisted values', () => {
    localStorage.setItem(PANE_DENSITY_STORAGE_KEY, 'huge');

    expect(readPersistedPaneDensity()).toBe('compact');
  });

  it('falls back when localStorage access throws', () => {
    const original = globalThis.localStorage;
    Object.defineProperty(globalThis, 'localStorage', {
      configurable: true,
      value: {
        getItem() {
          throw new Error('storage disabled');
        },
        setItem() {
          throw new Error('storage disabled');
        },
        removeItem() {
          throw new Error('storage disabled');
        },
      },
    });

    try {
      expect(readPersistedPaneDensity()).toBe('compact');
      expect(() => setPaneDensityMode('comfortable')).not.toThrow();
      expect(getPaneDensityMode()).toBe('comfortable');
    } finally {
      Object.defineProperty(globalThis, 'localStorage', {
        configurable: true,
        value: original,
      });
    }
  });
});
