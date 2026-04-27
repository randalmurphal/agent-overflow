import { describe, it, expect, beforeEach } from 'vitest';
import {
  DIFF_PANEL_MIN_WIDTH,
  getDiffPanelMaxWidth,
  getDiffPanelWidth,
  persistDiffPanelWidth,
  readPersistedDiffPanelWidth,
  resetDiffPanelLayoutForTest,
  setDiffPanelWidthLive,
  setDiffPanelWidth,
} from './diffPanelLayout.svelte';

describe('diffPanelLayout', () => {
  beforeEach(() => {
    resetDiffPanelLayoutForTest();
  });

  it('reads the default when no persisted value exists', () => {
    localStorage.clear();
    expect(readPersistedDiffPanelWidth()).toBeGreaterThanOrEqual(DIFF_PANEL_MIN_WIDTH);
  });

  it('falls back to default on corrupt persisted value', () => {
    localStorage.setItem('agent-overflow:diff-panel:width', 'not-a-number');
    expect(Number.isFinite(readPersistedDiffPanelWidth())).toBe(true);
  });

  it('clamps live updates to the min/max range', () => {
    setDiffPanelWidthLive(0);
    expect(getDiffPanelWidth()).toBeGreaterThanOrEqual(DIFF_PANEL_MIN_WIDTH);

    const maxAllowed = getDiffPanelMaxWidth();
    setDiffPanelWidthLive(Number.POSITIVE_INFINITY);
    expect(getDiffPanelWidth()).toBeLessThanOrEqual(maxAllowed);
  });

  it('persists width to localStorage on flush', () => {
    const desired = Math.max(DIFF_PANEL_MIN_WIDTH, Math.min(getDiffPanelMaxWidth(), 400));
    setDiffPanelWidthLive(desired);
    persistDiffPanelWidth();
    expect(localStorage.getItem('agent-overflow:diff-panel:width')).toBe(String(desired));
  });

  it('setDiffPanelWidth updates and persists in one call', () => {
    const desired = Math.max(DIFF_PANEL_MIN_WIDTH, Math.min(getDiffPanelMaxWidth(), 390));
    setDiffPanelWidth(desired);
    expect(getDiffPanelWidth()).toBe(desired);
    expect(localStorage.getItem('agent-overflow:diff-panel:width')).toBe(String(desired));
  });

  it('keeps width separate from the other RHS panels', () => {
    const desired = Math.max(DIFF_PANEL_MIN_WIDTH, Math.min(getDiffPanelMaxWidth(), 390));
    setDiffPanelWidth(desired);
    expect(localStorage.getItem('agent-overflow:diff-panel:width')).toBe(String(desired));
    expect(localStorage.getItem('agent-overflow:diff-sidebar:width')).toBeNull();
    expect(localStorage.getItem('agent-overflow:plan-sidebar:width')).toBeNull();
  });
});
