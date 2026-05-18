import { beforeEach, describe, expect, it } from 'vitest';
import {
  createRhsPanelSlot,
  RHS_PANEL_DEFAULT_WIDTH,
  RHS_PANEL_LRU_CAP,
  RHS_PANEL_MIN_WIDTH,
} from './rhsPanelSlot.svelte';
import { resetLayoutMetricsForTest, setPaneWidth } from './layoutMetrics.svelte';

const diffUI = {
  viewMode: 'split' as const,
  wordWrap: true,
  expandedFiles: ['src/foo.ts'],
  scrollTop: 120,
};

describe('createRhsPanelSlot', () => {
  beforeEach(() => {
    resetLayoutMetricsForTest();
    Object.defineProperty(window, 'innerWidth', {
      configurable: true,
      writable: true,
      value: 1400,
    });
  });

  it('starts closed at the shared default width', () => {
    const slot = createRhsPanelSlot('main');
    expect(slot.activePanel).toBeNull();
    expect(slot.width).toBe(RHS_PANEL_DEFAULT_WIDTH);
  });

  it('restores the active panel and width for a thread', () => {
    const slot = createRhsPanelSlot('main');
    slot.open({ kind: 'plan' });
    slot.setWidthLive(RHS_PANEL_DEFAULT_WIDTH + 40);
    slot.snapshotForThread('thread-a');

    slot.restoreForThread('thread-a');

    expect(slot.activePanel).toEqual({ kind: 'plan' });
    expect(slot.width).toBe(RHS_PANEL_DEFAULT_WIDTH + 40);
  });

  it('restores the design preview panel as a normal RHS variant', () => {
    const slot = createRhsPanelSlot('main');
    slot.open({ kind: 'design-preview' });
    slot.setWidthLive(RHS_PANEL_DEFAULT_WIDTH + 30);
    slot.snapshotForThread('thread-a');

    slot.restoreForThread('thread-a');

    expect(slot.activePanel).toEqual({ kind: 'design-preview' });
    expect(slot.width).toBe(RHS_PANEL_DEFAULT_WIDTH + 30);
  });

  it('keeps width when explicitly closing but does not restore a panel', () => {
    const slot = createRhsPanelSlot('main');
    slot.open({ kind: 'diff-checkpoint' });
    slot.setWidthLive(RHS_PANEL_DEFAULT_WIDTH + 20);

    slot.closeForThread('thread-a');
    slot.restoreForThread('thread-a');

    expect(slot.activePanel).toBeNull();
    expect(slot.width).toBe(RHS_PANEL_DEFAULT_WIDTH + 20);
  });

  it('restores diff-payload UI state once', () => {
    const slot = createRhsPanelSlot('main');
    slot.open({ kind: 'diff-payload', payloadId: 'payload-1', filePath: 'src/foo.ts' });
    slot.recordDiffPayloadUI(diffUI);
    slot.snapshotForThread('thread-a');

    slot.restoreForThread('thread-a');

    expect(slot.activePanel).toEqual({
      kind: 'diff-payload',
      payloadId: 'payload-1',
      filePath: 'src/foo.ts',
    });
    expect(slot.consumeDiffPayloadRestore()).toEqual(diffUI);
    expect(slot.consumeDiffPayloadRestore()).toBeNull();
  });

  it('keeps widths isolated by thread', () => {
    const slot = createRhsPanelSlot('main');
    slot.open({ kind: 'plan' });
    slot.setWidthLive(620);
    slot.snapshotForThread('thread-a');

    slot.restoreForThread('thread-b');
    expect(slot.width).toBe(RHS_PANEL_DEFAULT_WIDTH);

    slot.open({ kind: 'diff-checkpoint' });
    slot.setWidthLive(590);
    slot.snapshotForThread('thread-b');

    slot.restoreForThread('thread-a');
    expect(slot.width).toBe(620);
    expect(slot.activePanel).toEqual({ kind: 'plan' });

    slot.restoreForThread('thread-b');
    expect(slot.width).toBe(590);
    expect(slot.activePanel).toEqual({ kind: 'diff-checkpoint' });
  });

  it('clamps width below the minimum', () => {
    const slot = createRhsPanelSlot('main');
    slot.setWidthLive(1);
    expect(slot.width).toBe(RHS_PANEL_MIN_WIDTH);
  });

  it('clamps width against the owning pane width', () => {
    setPaneWidth('left', 1000);
    setPaneWidth('right', 1400);
    const left = createRhsPanelSlot('left');
    const right = createRhsPanelSlot('right');

    left.setWidthLive(900);
    right.setWidthLive(900);

    expect(left.getMaxWidth()).toBe(500);
    expect(left.width).toBe(500);
    expect(right.getMaxWidth()).toBe(900);
    expect(right.width).toBe(900);
  });

  it('re-clamps the visible width when the owning pane narrows', () => {
    setPaneWidth('main', 1600);
    const slot = createRhsPanelSlot('main');
    slot.setWidthLive(900);
    expect(slot.width).toBe(900);

    setPaneWidth('main', 1000);

    expect(slot.getMaxWidth()).toBe(500);
    expect(slot.width).toBe(500);
  });

  it('caps stored thread snapshots', () => {
    const slot = createRhsPanelSlot('main');
    for (let i = 0; i < RHS_PANEL_LRU_CAP + 5; i += 1) {
      slot.open({ kind: 'plan' });
      slot.snapshotForThread(`thread-${i}`);
    }
    expect(slot.snapshotCount).toBe(RHS_PANEL_LRU_CAP);

    slot.restoreForThread('thread-0');
    expect(slot.activePanel).toBeNull();

    slot.restoreForThread('thread-5');
    expect(slot.activePanel).toEqual({ kind: 'plan' });
  });
});
