import { beforeEach, describe, expect, it } from 'vitest';
import {
  addPaneLayoutItem,
  averagePaneRatio,
  getPaneLayoutItems,
  movePaneLayoutItem,
  movePaneLayoutItemToIndex,
  paneBlockRangeAt,
  removePaneLayoutItem,
  resetPaneLayoutForTest,
  resizeAdjacentPaneLayoutItems,
  setPaneLayoutItemsForTest,
  type PaneLayoutItem,
} from './paneLayout.svelte';

function thread(paneId: string): PaneLayoutItem {
  return { id: paneId, paneId, kind: 'thread', ratio: 1 };
}

function review(sourcePaneId: string): PaneLayoutItem {
  const paneId = `review-${sourcePaneId}`;
  return { id: paneId, paneId, kind: 'review', ratio: 1, sourcePaneId };
}

describe('paneLayout store', () => {
  beforeEach(() => {
    resetPaneLayoutForTest();
  });

  it('allows the layout to become empty', () => {
    removePaneLayoutItem('main');

    expect(getPaneLayoutItems()).toEqual([]);
  });

  it('adds a pane at the requested position', () => {
    addPaneLayoutItem({ id: 'right', paneId: 'right', kind: 'thread', ratio: 1 });
    addPaneLayoutItem({ id: 'middle', paneId: 'middle', kind: 'thread', ratio: 1 }, 1);

    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['main', 'middle', 'right']);
  });

  it('computes the average ratio for new panes', () => {
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', ratio: 0.625 },
      { id: 'right', paneId: 'right', kind: 'thread', ratio: 0.375 },
    ]);

    expect(averagePaneRatio()).toBeCloseTo(0.5);
  });

  it('moves panes by one slot and clamps at the edges', () => {
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', ratio: 1 },
      { id: 'middle', paneId: 'middle', kind: 'thread', ratio: 1 },
      { id: 'right', paneId: 'right', kind: 'thread', ratio: 1 },
    ]);

    movePaneLayoutItem('middle', -1);
    movePaneLayoutItem('middle', -1);
    movePaneLayoutItem('right', 1);

    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['middle', 'left', 'right']);
  });

  it('moves panes to a post-removal insert index', () => {
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', ratio: 1 },
      { id: 'middle', paneId: 'middle', kind: 'thread', ratio: 1 },
      { id: 'right', paneId: 'right', kind: 'thread', ratio: 1 },
    ]);

    movePaneLayoutItemToIndex('left', 1);

    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['middle', 'left', 'right']);
  });

  it('resnaps multiple companions after their source and drops orphans', () => {
    setPaneLayoutItemsForTest([
      { id: 'source', paneId: 'source', kind: 'thread', ratio: 1 },
      { id: 'other', paneId: 'other', kind: 'thread', ratio: 1 },
      { id: 'take-control-source', paneId: 'take-control-source', kind: 'take-control', ratio: 1, sourcePaneId: 'source' },
      { id: 'plan-source', paneId: 'plan-source', kind: 'plan', ratio: 1, sourcePaneId: 'source' },
      { id: 'design-preview-source', paneId: 'design-preview-source', kind: 'design-preview', ratio: 1, sourcePaneId: 'source' },
      { id: 'review-source', paneId: 'review-source', kind: 'review', ratio: 1, sourcePaneId: 'source' },
      { id: 'plan-missing', paneId: 'plan-missing', kind: 'plan', ratio: 1, sourcePaneId: 'missing' },
    ]);

    movePaneLayoutItemToIndex('other', 0);

    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual([
      'other',
      'source',
      'take-control-source',
      'plan-source',
      'design-preview-source',
      'review-source',
    ]);
  });

  it('inserting a thread pane inside a source+companion block lands after the block', () => {
    setPaneLayoutItemsForTest([thread('a'), review('a'), thread('b')]);

    // "Open to the right of the focused pane" computes focusedIndex + 1 —
    // between the source and its companion. The add must not split them.
    addPaneLayoutItem(thread('new'), 1);

    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual([
      'a',
      'review-a',
      'new',
      'b',
    ]);
  });

  it('moves a source pane and its companions as one unit', () => {
    setPaneLayoutItemsForTest([thread('a'), review('a'), thread('b'), thread('c')]);

    // One step right crosses the whole neighbor, not the own companion
    // (a single-slot swap with review-a would be undone by the resnap).
    movePaneLayoutItem('a', 1);
    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual([
      'b',
      'a',
      'review-a',
      'c',
    ]);

    movePaneLayoutItem('a', 1);
    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual([
      'b',
      'c',
      'a',
      'review-a',
    ]);

    // Clamped at the right edge.
    movePaneLayoutItem('a', 1);
    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual([
      'b',
      'c',
      'a',
      'review-a',
    ]);
  });

  it('moves a plain pane across a neighboring block in one step', () => {
    setPaneLayoutItemsForTest([thread('a'), review('a'), thread('b')]);

    movePaneLayoutItem('b', -1);

    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual([
      'b',
      'a',
      'review-a',
    ]);
  });

  it('reports block ranges for leads, companions, and plain panes', () => {
    setPaneLayoutItemsForTest([
      thread('a'),
      review('a'),
      { id: 'plan-a', paneId: 'plan-a', kind: 'plan', ratio: 1, sourcePaneId: 'a' },
      thread('b'),
    ]);
    const items = getPaneLayoutItems();

    expect(paneBlockRangeAt(items, 0)).toEqual({ start: 0, end: 2 });
    expect(paneBlockRangeAt(items, 1)).toEqual({ start: 0, end: 2 });
    expect(paneBlockRangeAt(items, 2)).toEqual({ start: 0, end: 2 });
    expect(paneBlockRangeAt(items, 3)).toEqual({ start: 3, end: 3 });
  });

  it('trades ratio between adjacent panes without changing their combined ratio', () => {
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', ratio: 1 },
      { id: 'right', paneId: 'right', kind: 'thread', ratio: 1 },
    ]);

    resizeAdjacentPaneLayoutItems('left', 'right', 800, 800, 200, 560);

    const [left, right] = getPaneLayoutItems();
    expect(left.ratio).toBeCloseTo(1.25);
    expect(right.ratio).toBeCloseTo(0.75);
    expect(left.ratio + right.ratio).toBeCloseTo(2);
  });
});
