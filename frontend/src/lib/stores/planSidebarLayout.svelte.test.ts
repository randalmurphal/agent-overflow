import { beforeEach, describe, expect, it } from 'vitest';
import {
  PLAN_SIDEBAR_MIN_WIDTH,
  getPlanSidebarMaxWidth,
  getPlanSidebarWidth,
  persistPlanSidebarWidth,
  readPersistedPlanSidebarWidth,
  resetPlanSidebarLayoutForTest,
  setPlanSidebarWidth,
  setPlanSidebarWidthLive,
} from './planSidebarLayout.svelte';

const STORAGE_KEY = 'agent-overflow:plan-sidebar:width';

// jsdom defaults to a 1024-wide window, so the viewport-derived max is
// ~384 (1024 - 640 main reserve). Tests stay below that.
const SAFE_MID = PLAN_SIDEBAR_MIN_WIDTH + 30;

describe('plan sidebar layout store', () => {
  beforeEach(() => {
    resetPlanSidebarLayoutForTest();
  });

  it('defaults to 440 when nothing is persisted', () => {
    expect(getPlanSidebarWidth()).toBe(440);
  });

  it('setPlanSidebarWidth updates state and persists', () => {
    setPlanSidebarWidth(SAFE_MID);
    expect(getPlanSidebarWidth()).toBe(SAFE_MID);
    expect(localStorage.getItem(STORAGE_KEY)).toBe(String(SAFE_MID));
  });

  it('clamps below the minimum', () => {
    setPlanSidebarWidth(40);
    expect(getPlanSidebarWidth()).toBe(PLAN_SIDEBAR_MIN_WIDTH);
  });

  it('clamps above the main-content reserve', () => {
    const max = getPlanSidebarMaxWidth();
    setPlanSidebarWidth(max + 500);
    expect(getPlanSidebarWidth()).toBe(max);
  });

  it('rounds fractional input to a whole pixel', () => {
    setPlanSidebarWidth(SAFE_MID + 0.7);
    expect(getPlanSidebarWidth()).toBe(SAFE_MID + 1);
  });

  describe('live / persist split', () => {
    it('setPlanSidebarWidthLive updates state without touching storage', () => {
      setPlanSidebarWidthLive(SAFE_MID);
      expect(getPlanSidebarWidth()).toBe(SAFE_MID);
      expect(localStorage.getItem(STORAGE_KEY)).toBeNull();
    });

    it('persistPlanSidebarWidth flushes the current in-memory width', () => {
      setPlanSidebarWidthLive(SAFE_MID);
      expect(localStorage.getItem(STORAGE_KEY)).toBeNull();
      persistPlanSidebarWidth();
      expect(localStorage.getItem(STORAGE_KEY)).toBe(String(SAFE_MID));
    });

    it('repeated live updates do not write even while dragging', () => {
      for (let px = PLAN_SIDEBAR_MIN_WIDTH + 1; px < PLAN_SIDEBAR_MIN_WIDTH + 20; px++) setPlanSidebarWidthLive(px);
      expect(localStorage.getItem(STORAGE_KEY)).toBeNull();
      expect(getPlanSidebarWidth()).toBe(PLAN_SIDEBAR_MIN_WIDTH + 19);
    });
  });

  describe('readPersistedPlanSidebarWidth', () => {
    it('returns default when no value is stored', () => {
      expect(readPersistedPlanSidebarWidth()).toBe(440);
    });

    it('returns default when the stored value is garbage', () => {
      localStorage.setItem(STORAGE_KEY, 'garbage');
      expect(readPersistedPlanSidebarWidth()).toBe(440);
    });

    it('returns default when the stored value is empty', () => {
      localStorage.setItem(STORAGE_KEY, '');
      expect(readPersistedPlanSidebarWidth()).toBe(440);
    });

    it('clamps a stored value below the minimum', () => {
      localStorage.setItem(STORAGE_KEY, '40');
      expect(readPersistedPlanSidebarWidth()).toBe(PLAN_SIDEBAR_MIN_WIDTH);
    });

    it('round-trips a valid stored value', () => {
      localStorage.setItem(STORAGE_KEY, String(SAFE_MID));
      expect(readPersistedPlanSidebarWidth()).toBe(SAFE_MID);
    });
  });
});
