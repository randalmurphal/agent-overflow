import { beforeEach, describe, expect, it } from 'vitest';
import {
  createRhsPanelSlot,
  RHS_PANEL_DEFAULT_WIDTH,
  RHS_PANEL_LRU_CAP,
  RHS_PANEL_MIN_WIDTH,
} from './rhsPanelSlot.svelte';

const diffUI = {
  viewMode: 'split' as const,
  wordWrap: true,
  expandedFiles: ['src/foo.ts'],
  scrollTop: 120,
};

describe('createRhsPanelSlot', () => {
  beforeEach(() => {
    Object.defineProperty(window, 'innerWidth', {
      configurable: true,
      writable: true,
      value: 1400,
    });
  });

  it('starts closed at the shared default width', () => {
    const slot = createRhsPanelSlot();
    expect(slot.activePanel).toBeNull();
    expect(slot.width).toBe(RHS_PANEL_DEFAULT_WIDTH);
  });

  it('restores the active panel and width for a thread', () => {
    const slot = createRhsPanelSlot();
    slot.open({ kind: 'plan' });
    slot.setWidthLive(RHS_PANEL_DEFAULT_WIDTH + 40);
    slot.snapshotForThread('thread-a');

    slot.restoreForThread('thread-a');

    expect(slot.activePanel).toEqual({ kind: 'plan' });
    expect(slot.width).toBe(RHS_PANEL_DEFAULT_WIDTH + 40);
  });

  it('keeps width when explicitly closing but does not restore a panel', () => {
    const slot = createRhsPanelSlot();
    slot.open({ kind: 'diff-checkpoint' });
    slot.setWidthLive(RHS_PANEL_DEFAULT_WIDTH + 20);

    slot.closeForThread('thread-a');
    slot.restoreForThread('thread-a');

    expect(slot.activePanel).toBeNull();
    expect(slot.width).toBe(RHS_PANEL_DEFAULT_WIDTH + 20);
  });

  it('restores diff-payload UI state once', () => {
    const slot = createRhsPanelSlot();
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
    const slot = createRhsPanelSlot();
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
    const slot = createRhsPanelSlot();
    slot.setWidthLive(1);
    expect(slot.width).toBe(RHS_PANEL_MIN_WIDTH);
  });

  it('caps stored thread snapshots', () => {
    const slot = createRhsPanelSlot();
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
