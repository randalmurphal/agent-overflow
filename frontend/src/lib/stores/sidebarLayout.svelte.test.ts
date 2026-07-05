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
import { appStorageGet, appStorageSet, resetAppStorageForTest } from './appStorage';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';

const WIDTH_KEY = 'sidebar:width';
const LEGACY_STORAGE_KEY = 'agent-overflow:sidebar:width';

describe('sidebar layout store', () => {
  beforeEach(() => {
    resetBindingMocks();
    setBindingMock('SetUIState', async () => null);
    setBindingMock('DeleteUIState', async () => null);
    localStorage.removeItem(LEGACY_STORAGE_KEY);
    resetAppStorageForTest();
    resetSidebarLayoutForTest();
  });

  it('defaults to 280 when nothing is persisted', () => {
    expect(getSidebarWidth()).toBe(280);
  });

  it('setSidebarWidth updates state and persists', () => {
    setSidebarWidth(320);
    expect(getSidebarWidth()).toBe(320);
    expect(appStorageGet(WIDTH_KEY)).toBe('320');
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
      expect(appStorageGet(WIDTH_KEY)).toBeNull();
    });

    it('persistSidebarWidth flushes the current in-memory width', () => {
      setSidebarWidthLive(340);
      expect(appStorageGet(WIDTH_KEY)).toBeNull();
      persistSidebarWidth();
      expect(appStorageGet(WIDTH_KEY)).toBe('340');
    });

    it('repeated live updates do not write even while dragging', () => {
      for (let px = 201; px < 220; px++) setSidebarWidthLive(px);
      expect(appStorageGet(WIDTH_KEY)).toBeNull();
      expect(getSidebarWidth()).toBe(219);
    });
  });

  describe('readPersistedWidth', () => {
    it('returns default when no value is stored', () => {
      expect(readPersistedWidth()).toBe(280);
    });

    it('returns default when the stored value is garbage', () => {
      appStorageSet(WIDTH_KEY, 'garbage');
      expect(readPersistedWidth()).toBe(280);
    });

    it('clamps a stored value below the minimum', () => {
      appStorageSet(WIDTH_KEY, '10');
      expect(readPersistedWidth()).toBe(SIDEBAR_MIN_WIDTH);
    });

    it('round-trips a valid stored value', () => {
      appStorageSet(WIDTH_KEY, '310');
      expect(readPersistedWidth()).toBe(310);
    });

    it('adopts a legacy localStorage width when the bucket is empty', () => {
      localStorage.setItem(LEGACY_STORAGE_KEY, '310');
      expect(readPersistedWidth()).toBe(310);
      // Adoption moves the value into the bucket and drops the legacy key.
      expect(appStorageGet(WIDTH_KEY)).toBe('310');
      expect(localStorage.getItem(LEGACY_STORAGE_KEY)).toBeNull();
    });

    it('rejects a corrupt legacy value and falls back to default', () => {
      localStorage.setItem(LEGACY_STORAGE_KEY, 'garbage');
      expect(readPersistedWidth()).toBe(280);
      expect(appStorageGet(WIDTH_KEY)).toBeNull();
    });
  });
});
