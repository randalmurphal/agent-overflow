import { beforeEach, describe, expect, it } from 'vitest';
import {
  addPaneLayoutItem,
  averagePaneRatio,
  getPaneLayoutItems,
  movePaneLayoutItem,
  movePaneLayoutItemToIndex,
  removePaneLayoutItem,
  resetPaneLayoutForTest,
  resizeAdjacentPaneLayoutItems,
  setPaneLayoutItemsForTest,
} from './paneLayout.svelte';

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
