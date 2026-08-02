import { beforeEach, describe, expect, it } from 'vitest';
import {
  SIDEBAR_MIN_WIDTH,
  getSidebarMaxWidth,
  getSidebarWidth,
  isSidebarCollapsed,
  persistSidebarWidth,
  readPersistedCollapsed,
  readPersistedWidth,
  resetSidebarLayoutForTest,
  setSidebarCollapsed,
  setSidebarWidth,
  setSidebarWidthLive,
  syncSidebarLayoutFromAppStorage,
  toggleSidebarCollapsed,
} from './sidebarLayout.svelte';
import { appStorageGet, appStorageSet, resetAppStorageForTest } from './appStorage';
import { setAppShellWidth, resetLayoutMetricsForTest } from './layoutMetrics.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';

const WIDTH_KEY = 'sidebar:width';
const COLLAPSED_KEY = 'sidebar:collapsed';
const LEGACY_STORAGE_KEY = 'agent-overflow:sidebar:width';

describe('sidebar layout store', () => {
  beforeEach(() => {
    resetBindingMocks();
    setBindingMock('SetUIState', async () => null);
    setBindingMock('DeleteUIState', async () => null);
    localStorage.removeItem(LEGACY_STORAGE_KEY);
    resetAppStorageForTest();
    resetLayoutMetricsForTest();
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

  describe('collapse / expand', () => {
    it('starts expanded', () => {
      expect(isSidebarCollapsed()).toBe(false);
    });

    it('collapsing hides the sidebar without touching the stored width', () => {
      setSidebarWidth(340);
      setSidebarCollapsed(true);
      expect(isSidebarCollapsed()).toBe(true);
      expect(getSidebarWidth()).toBe(340);
    });

    it('expanding restores the previous width, not the minimum', () => {
      setSidebarWidth(340);
      setSidebarCollapsed(true);
      setSidebarCollapsed(false);
      expect(isSidebarCollapsed()).toBe(false);
      expect(getSidebarWidth()).toBe(340);
    });

    it('survives a collapse → expand → collapse round trip', () => {
      setSidebarWidth(320);
      toggleSidebarCollapsed();
      expect(isSidebarCollapsed()).toBe(true);
      toggleSidebarCollapsed();
      expect(isSidebarCollapsed()).toBe(false);
      toggleSidebarCollapsed();
      expect(isSidebarCollapsed()).toBe(true);
      expect(getSidebarWidth()).toBe(320);
    });

    it('persists both directions', () => {
      setSidebarCollapsed(true);
      expect(appStorageGet(COLLAPSED_KEY)).toBe('1');
      setSidebarCollapsed(false);
      expect(appStorageGet(COLLAPSED_KEY)).toBe('0');
    });

    it('collapsing flushes an unpersisted drag width so expanding restores it', () => {
      // The live setter is what a drag calls; collapsing mid-drag tears
      // the resizer down before its pointerup flush ever runs.
      setSidebarWidthLive(410);
      expect(appStorageGet(WIDTH_KEY)).toBeNull();
      setSidebarCollapsed(true);
      expect(appStorageGet(WIDTH_KEY)).toBe('410');
      setSidebarCollapsed(false);
      expect(getSidebarWidth()).toBe(410);
    });

    it('re-collapsing an already-collapsed sidebar still flushes the width', () => {
      setSidebarCollapsed(true);
      setSidebarWidthLive(360);
      setSidebarCollapsed(true);
      expect(appStorageGet(WIDTH_KEY)).toBe('360');
    });

    it('expanding re-clamps a width the viewport no longer allows', () => {
      setAppShellWidth(1600);
      setSidebarWidth(600);
      expect(getSidebarWidth()).toBe(600);
      setSidebarCollapsed(true);
      // Window shrinks while nothing is rendering the sidebar's width,
      // so nothing has re-clamped it.
      setAppShellWidth(900);
      setSidebarCollapsed(false);
      expect(getSidebarWidth()).toBe(getSidebarMaxWidth());
      expect(appStorageGet(WIDTH_KEY)).toBe(String(getSidebarMaxWidth()));
    });

    it('expanding leaves a still-valid width alone', () => {
      setAppShellWidth(1600);
      setSidebarWidth(400);
      setSidebarCollapsed(true);
      setSidebarCollapsed(false);
      expect(getSidebarWidth()).toBe(400);
    });
  });

  describe('readPersistedCollapsed', () => {
    it('defaults to expanded when nothing is stored', () => {
      expect(readPersistedCollapsed()).toBe(false);
    });

    it('reads the collapsed marker', () => {
      appStorageSet(COLLAPSED_KEY, '1');
      expect(readPersistedCollapsed()).toBe(true);
    });

    it('treats any other stored value as expanded', () => {
      appStorageSet(COLLAPSED_KEY, 'garbage');
      expect(readPersistedCollapsed()).toBe(false);
    });
  });

  describe('syncSidebarLayoutFromAppStorage', () => {
    it('adopts the durable collapsed flag', () => {
      appStorageSet(COLLAPSED_KEY, '1');
      syncSidebarLayoutFromAppStorage();
      expect(isSidebarCollapsed()).toBe(true);
    });

    it('adopts a durable expanded flag over a collapsed in-memory state', () => {
      setSidebarCollapsed(true);
      appStorageSet(COLLAPSED_KEY, '0');
      syncSidebarLayoutFromAppStorage();
      expect(isSidebarCollapsed()).toBe(false);
    });

    it('still adopts the durable width', () => {
      appStorageSet(WIDTH_KEY, '312');
      syncSidebarLayoutFromAppStorage();
      expect(getSidebarWidth()).toBe(312);
    });

    it('leaves state alone when the bucket holds neither key', () => {
      setSidebarWidth(330);
      resetAppStorageForTest();
      syncSidebarLayoutFromAppStorage();
      expect(getSidebarWidth()).toBe(330);
      expect(isSidebarCollapsed()).toBe(false);
    });
  });
});
