import { describe, it, expect, beforeEach } from 'vitest';
import {
  DIFF_SIDEBAR_MIN_WIDTH,
  getDiffSidebarMaxWidth,
  getDiffSidebarWidth,
  persistDiffSidebarWidth,
  readPersistedDiffSidebarWidth,
  resetDiffSidebarLayoutForTest,
  setDiffSidebarWidthLive,
  setDiffSidebarWidth,
} from './diffSidebarLayout.svelte';

describe('diffSidebarLayout', () => {
  beforeEach(() => {
    resetDiffSidebarLayoutForTest();
  });

  it('reads the default when no persisted value exists', () => {
    localStorage.clear();
    expect(readPersistedDiffSidebarWidth()).toBeGreaterThanOrEqual(DIFF_SIDEBAR_MIN_WIDTH);
  });

  it('falls back to default on corrupt persisted value', () => {
    localStorage.setItem('agent-overflow:diff-sidebar:width', 'not-a-number');
    expect(Number.isFinite(readPersistedDiffSidebarWidth())).toBe(true);
  });

  it('clamps live updates to the min/max range', () => {
    setDiffSidebarWidthLive(0);
    expect(getDiffSidebarWidth()).toBeGreaterThanOrEqual(DIFF_SIDEBAR_MIN_WIDTH);

    const maxAllowed = getDiffSidebarMaxWidth();
    setDiffSidebarWidthLive(Number.POSITIVE_INFINITY);
    expect(getDiffSidebarWidth()).toBeLessThanOrEqual(maxAllowed);
  });

  it('persists width to localStorage on flush', () => {
    // Pick a value within both min and viewport-derived max so the
    // clamp in jsdom (window.innerWidth=1024 → max=384) doesn't
    // shadow what we asked for.
    const desired = Math.max(DIFF_SIDEBAR_MIN_WIDTH, Math.min(getDiffSidebarMaxWidth(), 380));
    setDiffSidebarWidthLive(desired);
    persistDiffSidebarWidth();
    expect(localStorage.getItem('agent-overflow:diff-sidebar:width')).toBe(String(desired));
  });

  it('setDiffSidebarWidth updates and persists in one call', () => {
    const desired = Math.max(DIFF_SIDEBAR_MIN_WIDTH, Math.min(getDiffSidebarMaxWidth(), 370));
    setDiffSidebarWidth(desired);
    expect(getDiffSidebarWidth()).toBe(desired);
    expect(localStorage.getItem('agent-overflow:diff-sidebar:width')).toBe(String(desired));
  });

  it('does not write the same value twice', () => {
    const desired = Math.max(DIFF_SIDEBAR_MIN_WIDTH, Math.min(getDiffSidebarMaxWidth(), 370));
    setDiffSidebarWidthLive(desired);
    const initial = getDiffSidebarWidth();
    setDiffSidebarWidthLive(desired);
    expect(getDiffSidebarWidth()).toBe(initial);
  });

  it('keeps width separate from PlanSidebar via different storage key', () => {
    const desired = Math.max(DIFF_SIDEBAR_MIN_WIDTH, Math.min(getDiffSidebarMaxWidth(), 370));
    setDiffSidebarWidth(desired);
    // PlanSidebar uses agent-overflow:plan-sidebar:width — confirm
    // the diff sidebar key is distinct
    expect(localStorage.getItem('agent-overflow:diff-sidebar:width')).toBe(String(desired));
    expect(localStorage.getItem('agent-overflow:plan-sidebar:width')).toBeNull();
  });
});
