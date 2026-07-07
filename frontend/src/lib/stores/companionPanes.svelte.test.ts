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
  companionForSource,
  getCompanionPane,
  installCompanionPanes,
  isCompanionOpen,
  openCompanion,
  resetCompanionPanesForTest,
  toggleCompanion,
} from './companionPanes.svelte';

function threadItem(paneId: string, ratio = 1): PaneLayoutItem {
  return { id: paneId, paneId, kind: 'thread', ratio };
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
    setPaneLayoutItemsForTest([threadItem('main', 1.5), threadItem('right')]);

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
    expect(getPaneLayoutItems()[1].ratio).toBe(1.5);
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

  it('cascade-closes companions when the source pane is destroyed', () => {
    setPaneLayoutItemsForTest([threadItem('p1')]);
    createPane('p1');
    openCompanion('p1', 'plan');
    openCompanion('p1', 'design-preview');
    openCompanion('p1', 'review');
    expect(paneIds()).toEqual(['p1', 'plan-p1', 'design-preview-p1', 'review-p1']);

    destroyPane('p1');

    expect(isCompanionOpen('p1', 'plan')).toBe(false);
    expect(isCompanionOpen('p1', 'design-preview')).toBe(false);
    expect(isCompanionOpen('p1', 'review')).toBe(false);
    expect(getPaneLayoutItems()).toEqual([]);
  });
});
