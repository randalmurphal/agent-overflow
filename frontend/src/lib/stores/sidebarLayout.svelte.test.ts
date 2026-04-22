import { beforeEach, describe, expect, it } from 'vitest';
import {
  SIDEBAR_MIN_WIDTH,
  getSidebarMaxWidth,
  getSidebarWidth,
  persistSidebarWidth,
  readPersistedWidth,
  resetSidebarLayoutForTest,
  setSidebarWidth,
  setSidebarWidthLive,
} from './sidebarLayout.svelte';

const STORAGE_KEY = 'agent-overflow:sidebar:width';

describe('sidebar layout store', () => {
  beforeEach(() => {
    resetSidebarLayoutForTest();
  });

  it('defaults to 280 when nothing is persisted', () => {
    expect(getSidebarWidth()).toBe(280);
  });

  it('setSidebarWidth updates state and persists', () => {
    setSidebarWidth(320);
    expect(getSidebarWidth()).toBe(320);
    expect(localStorage.getItem(STORAGE_KEY)).toBe('320');
  });

  it('clamps below the minimum', () => {
    setSidebarWidth(10);
    expect(getSidebarWidth()).toBe(SIDEBAR_MIN_WIDTH);
  });

  it('clamps above the main-content reserve', () => {
    const max = getSidebarMaxWidth();
    setSidebarWidth(max + 500);
    expect(getSidebarWidth()).toBe(max);
  });

  it('rounds fractional input to a whole pixel', () => {
    setSidebarWidth(301.7);
    expect(getSidebarWidth()).toBe(302);
  });

  describe('live / persist split', () => {
    it('setSidebarWidthLive updates state without touching storage', () => {
      setSidebarWidthLive(340);
      expect(getSidebarWidth()).toBe(340);
      expect(localStorage.getItem(STORAGE_KEY)).toBeNull();
    });

    it('persistSidebarWidth flushes the current in-memory width', () => {
      setSidebarWidthLive(340);
      expect(localStorage.getItem(STORAGE_KEY)).toBeNull();
      persistSidebarWidth();
      expect(localStorage.getItem(STORAGE_KEY)).toBe('340');
    });

    it('repeated live updates do not write even while dragging', () => {
      for (let px = 201; px < 220; px++) setSidebarWidthLive(px);
      expect(localStorage.getItem(STORAGE_KEY)).toBeNull();
      expect(getSidebarWidth()).toBe(219);
    });
  });

  describe('readPersistedWidth', () => {
    it('returns default when no value is stored', () => {
      expect(readPersistedWidth()).toBe(280);
    });

    it('returns default when the stored value is garbage', () => {
      localStorage.setItem(STORAGE_KEY, 'garbage');
      expect(readPersistedWidth()).toBe(280);
    });

    it('returns default when the stored value is empty', () => {
      localStorage.setItem(STORAGE_KEY, '');
      expect(readPersistedWidth()).toBe(280);
    });

    it('clamps a stored value below the minimum', () => {
      localStorage.setItem(STORAGE_KEY, '10');
      expect(readPersistedWidth()).toBe(SIDEBAR_MIN_WIDTH);
    });

    it('round-trips a valid stored value', () => {
      localStorage.setItem(STORAGE_KEY, '310');
      expect(readPersistedWidth()).toBe(310);
    });
  });
});
