import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  createPane,
  destroyPane,
  resetPanesForTest,
} from './panes.svelte';
import {
  getPaneLayoutItems,
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
  type PaneLayoutItem,
} from './paneLayout.svelte';
import {
  closeCompanion,
  closeCompanionsForSource,
  companionForSource,
  getCompanionPane,
  installCompanionPanes,
  isCompanionOpen,
  openCompanion,
  resetCompanionPanesForTest,
  toggleCompanion,
} from './companionPanes.svelte';

function threadItem(paneId: string, widthPx = 560): PaneLayoutItem {
  return { id: paneId, paneId, kind: 'thread', widthPx };
}

function paneIds(): string[] {
  return getPaneLayoutItems().map((item) => item.paneId);
}

beforeEach(() => {
  resetPanesForTest();
  resetCompanionPanesForTest();
  resetPaneLayoutForTest();
  installCompanionPanes();
});

afterEach(() => {
  resetCompanionPanesForTest();
  resetPanesForTest();
  resetPaneLayoutForTest();
});

describe('companionPanes store', () => {
  it('opens a companion after existing companions for the same source', () => {
    setPaneLayoutItemsForTest([threadItem('main', 900), threadItem('right')]);

    const plan = openCompanion('main', 'plan');
    const preview = openCompanion('main', 'design-preview');
    const review = openCompanion('main', 'review');

    expect(plan).toEqual({ paneId: 'plan-main', kind: 'plan', sourcePaneId: 'main' });
    expect(preview).toEqual({
      paneId: 'design-preview-main',
      kind: 'design-preview',
      sourcePaneId: 'main',
    });
    expect(review).toEqual({ paneId: 'review-main', kind: 'review', sourcePaneId: 'main' });
    expect(paneIds()).toEqual(['main', 'plan-main', 'design-preview-main', 'review-main', 'right']);
    expect(getPaneLayoutItems()[1].widthPx).toBe(900);
    expect(isCompanionOpen('main', 'plan')).toBe(true);
    expect(getCompanionPane('plan-main')).toEqual(plan);
    expect(companionForSource('main', 'design-preview')).toEqual(preview);
  });

  it('does not open a duplicate companion for the same source and kind', () => {
    setPaneLayoutItemsForTest([threadItem('main')]);

    const first = openCompanion('main', 'plan');
    const second = openCompanion('main', 'plan');

    expect(second).toBe(first);
    expect(getPaneLayoutItems().filter((item) => item.kind === 'plan')).toHaveLength(1);
  });

  it('toggles and closes companions without touching the source pane', () => {
    setPaneLayoutItemsForTest([threadItem('main')]);

    expect(toggleCompanion('main', 'plan')).toBe(true);
    expect(isCompanionOpen('main', 'plan')).toBe(true);

    expect(toggleCompanion('main', 'plan')).toBe(false);
    expect(isCompanionOpen('main', 'plan')).toBe(false);
    expect(paneIds()).toEqual(['main']);

    openCompanion('main', 'design-preview');
    closeCompanion('design-preview-main');
    expect(paneIds()).toEqual(['main']);
    expect(getCompanionPane('design-preview-main')).toBeNull();
  });

  it('refuses to open when the source pane is absent from the layout', () => {
    setPaneLayoutItemsForTest([threadItem('main')]);

    expect(openCompanion('ghost', 'plan')).toBeNull();
    expect(getPaneLayoutItems()).toEqual([threadItem('main')]);
  });

  it('take-control hugs its source, ahead of open panel companions', () => {
    setPaneLayoutItemsForTest([threadItem('main'), threadItem('right')]);
    openCompanion('main', 'plan');

    const takeControl = openCompanion('main', 'take-control');

    expect(takeControl).toEqual({
      paneId: 'take-control-main',
      kind: 'take-control',
      sourcePaneId: 'main',
    });
    // The shared top-border indicator reads source + terminal as one
    // entity, so nothing may sit between them.
    expect(paneIds()).toEqual(['main', 'take-control-main', 'plan-main', 'right']);

    // A panel companion opened afterwards appends after the run and
    // does not break the pairing.
    openCompanion('main', 'review');
    expect(paneIds()).toEqual(['main', 'take-control-main', 'plan-main', 'review-main', 'right']);
  });

  it('closes every companion for one source, leaving other sources alone', () => {
    setPaneLayoutItemsForTest([threadItem('main'), threadItem('right')]);
    openCompanion('main', 'plan');
    openCompanion('main', 'review');
    openCompanion('right', 'plan');

    closeCompanionsForSource('main');

    expect(isCompanionOpen('main', 'plan')).toBe(false);
    expect(isCompanionOpen('main', 'review')).toBe(false);
    expect(isCompanionOpen('right', 'plan')).toBe(true);
    expect(paneIds()).toEqual(['main', 'right', 'plan-right']);
  });

  it('cascade-closes companions when the source pane is destroyed', () => {
    setPaneLayoutItemsForTest([threadItem('p1')]);
    createPane('p1');
    openCompanion('p1', 'plan');
    openCompanion('p1', 'design-preview');
    openCompanion('p1', 'review');
    openCompanion('p1', 'take-control');
    expect(paneIds()).toEqual(['p1', 'take-control-p1', 'plan-p1', 'design-preview-p1', 'review-p1']);

    destroyPane('p1');

    expect(isCompanionOpen('p1', 'plan')).toBe(false);
    expect(isCompanionOpen('p1', 'design-preview')).toBe(false);
    expect(isCompanionOpen('p1', 'review')).toBe(false);
    expect(isCompanionOpen('p1', 'take-control')).toBe(false);
    expect(getPaneLayoutItems()).toEqual([]);
  });
});
