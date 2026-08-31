import { beforeEach, describe, expect, it } from 'vitest';
import {
  addPaneLayoutItem,
  applyPaneBoundaryDrag,
  averagePaneWidthPx,
  equalizePaneWidths,
  getPaneLayoutItems,
  minAnchorPaneLayoutWidths,
  movePaneLayoutItem,
  movePaneLayoutItemToIndex,
  isCompanionKind,
  paneBlockRangeAt,
  removePaneLayoutItem,
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
  type PaneLayoutItem,
} from './paneLayout.svelte';
import { resetLayoutMetricsForTest, setPaneHostWidth } from './layoutMetrics.svelte';

function thread(paneId: string): PaneLayoutItem {
  return { id: paneId, paneId, kind: 'thread', widthPx: 560 };
}

function review(sourcePaneId: string): PaneLayoutItem {
  const paneId = `review-${sourcePaneId}`;
  return { id: paneId, paneId, kind: 'review', widthPx: 560, sourcePaneId };
}

describe('paneLayout store', () => {
  beforeEach(() => {
    resetPaneLayoutForTest();
    resetLayoutMetricsForTest();
  });

  it('classifies every companion kind, and only those', () => {
    expect(isCompanionKind('agent')).toBe(true);
    expect(isCompanionKind('review')).toBe(true);
    expect(isCompanionKind('plan')).toBe(true);
    expect(isCompanionKind('take-control')).toBe(true);
    expect(isCompanionKind('thread')).toBe(false);
  });

  it('deep-copies an agent item\'s persisted scope on every clone', () => {
    const scope = {
      scopeItemId: 'launch-1',
      breadcrumb: [
        { itemId: '', label: 'main' },
        { itemId: 'launch-1', label: 'code-review' },
      ],
    };
    addPaneLayoutItem({
      id: 'agent-main',
      paneId: 'agent-main',
      kind: 'agent',
      widthPx: 560,
      sourcePaneId: 'main',
      agentScope: scope,
    });

    const stored = getPaneLayoutItems().find((item) => item.paneId === 'agent-main');
    expect(stored?.agentScope).toEqual(scope);
    // A shared breadcrumb array would let one item's trail mutate another's.
    expect(stored?.agentScope).not.toBe(scope);
    expect(stored?.agentScope?.breadcrumb[0]).not.toBe(scope.breadcrumb[0]);

    scope.breadcrumb.push({ itemId: 'launch-2', label: 'Angle B' });
    expect(getPaneLayoutItems().find((item) => item.paneId === 'agent-main')?.agentScope?.breadcrumb)
      .toHaveLength(2);
  });

  it('allows the layout to become empty', () => {
    removePaneLayoutItem('main');

    expect(getPaneLayoutItems()).toEqual([]);
  });

  it('adds a pane at the requested position', () => {
    addPaneLayoutItem({ id: 'right', paneId: 'right', kind: 'thread', widthPx: 1 });
    addPaneLayoutItem({ id: 'middle', paneId: 'middle', kind: 'thread', widthPx: 1 }, 1);

    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['main', 'middle', 'right']);
  });

  it('computes the average width for new panes', () => {
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', widthPx: 700 },
      { id: 'right', paneId: 'right', kind: 'thread', widthPx: 900 },
    ]);

    expect(averagePaneWidthPx()).toBeCloseTo(800);
  });

  it('moves panes by one slot and clamps at the edges', () => {
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 },
      { id: 'middle', paneId: 'middle', kind: 'thread', widthPx: 1 },
      { id: 'right', paneId: 'right', kind: 'thread', widthPx: 1 },
    ]);

    movePaneLayoutItem('middle', -1);
    movePaneLayoutItem('middle', -1);
    movePaneLayoutItem('right', 1);

    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['middle', 'left', 'right']);
  });

  it('moves panes to a post-removal insert index', () => {
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', widthPx: 1 },
      { id: 'middle', paneId: 'middle', kind: 'thread', widthPx: 1 },
      { id: 'right', paneId: 'right', kind: 'thread', widthPx: 1 },
    ]);

    movePaneLayoutItemToIndex('left', 1);

    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['middle', 'left', 'right']);
  });

  it('resnaps multiple companions after their source and drops orphans', () => {
    setPaneLayoutItemsForTest([
      { id: 'source', paneId: 'source', kind: 'thread', widthPx: 1 },
      { id: 'other', paneId: 'other', kind: 'thread', widthPx: 1 },
      { id: 'take-control-source', paneId: 'take-control-source', kind: 'take-control', widthPx: 1, sourcePaneId: 'source' },
      { id: 'plan-source', paneId: 'plan-source', kind: 'plan', widthPx: 1, sourcePaneId: 'source' },
      { id: 'review-source', paneId: 'review-source', kind: 'review', widthPx: 1, sourcePaneId: 'source' },
      { id: 'plan-missing', paneId: 'plan-missing', kind: 'plan', widthPx: 1, sourcePaneId: 'missing' },
    ]);

    movePaneLayoutItemToIndex('other', 0);

    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual([
      'other',
      'source',
      'take-control-source',
      'plan-source',
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
      { id: 'plan-a', paneId: 'plan-a', kind: 'plan', widthPx: 1, sourcePaneId: 'a' },
      thread('b'),
    ]);
    const items = getPaneLayoutItems();

    expect(paneBlockRangeAt(items, 0)).toEqual({ start: 0, end: 2 });
    expect(paneBlockRangeAt(items, 1)).toEqual({ start: 0, end: 2 });
    expect(paneBlockRangeAt(items, 2)).toEqual({ start: 0, end: 2 });
    expect(paneBlockRangeAt(items, 3)).toEqual({ start: 3, end: 3 });
  });

  it('applies a boundary drag from the drag-start snapshot, not accumulated state', () => {
    setPaneLayoutItemsForTest([
      { id: 'left', paneId: 'left', kind: 'thread', widthPx: 800 },
      { id: 'right', paneId: 'right', kind: 'thread', widthPx: 800 },
    ]);
    const startWidths = new Map([['left', 800], ['right', 800]]);
    const drag = (deltaPx: number) => applyPaneBoundaryDrag({
      leftPaneId: 'left',
      rightPaneId: 'right',
      startWidths,
      deltaPx,
      minPaneWidth: 560,
      overflowPx: 0,
      zeroSum: false,
    });

    drag(100);
    expect(getPaneLayoutItems().map((item) => item.widthPx)).toEqual([900, 700]);

    // Same gesture, pointer moved back: retraces instead of compounding.
    drag(0);
    expect(getPaneLayoutItems().map((item) => item.widthPx)).toEqual([800, 800]);
  });

  it('rejects a boundary drag whose panes are not adjacent in the layout', () => {
    setPaneLayoutItemsForTest([thread('a'), thread('b'), thread('c')]);

    applyPaneBoundaryDrag({
      leftPaneId: 'a',
      rightPaneId: 'c',
      startWidths: new Map([['a', 800], ['b', 800], ['c', 800]]),
      deltaPx: 100,
      minPaneWidth: 560,
      overflowPx: 0,
      zeroSum: false,
    });

    expect(getPaneLayoutItems().map((item) => item.widthPx)).toEqual([560, 560, 560]);
  });

  it('treats a null right pane as the end handle and requires the last pane', () => {
    setPaneLayoutItemsForTest([thread('a'), thread('b')]);
    const startWidths = new Map([['a', 560], ['b', 560]]);

    // Not the last pane: rejected.
    applyPaneBoundaryDrag({
      leftPaneId: 'a',
      rightPaneId: null,
      startWidths,
      deltaPx: 200,
      minPaneWidth: 560,
      overflowPx: 40,
      zeroSum: false,
    });
    expect(getPaneLayoutItems().map((item) => item.widthPx)).toEqual([560, 560]);

    applyPaneBoundaryDrag({
      leftPaneId: 'b',
      rightPaneId: null,
      startWidths,
      deltaPx: 200,
      minPaneWidth: 560,
      overflowPx: 40,
      zeroSum: false,
    });
    expect(getPaneLayoutItems().map((item) => item.widthPx)).toEqual([560, 760]);
  });

  it('equalizes every pane width to the density minimum', () => {
    setPaneLayoutItemsForTest([
      { id: 'a', paneId: 'a', kind: 'thread', widthPx: 900 },
      { id: 'b', paneId: 'b', kind: 'thread', widthPx: 1400 },
    ]);

    equalizePaneWidths(560);

    expect(getPaneLayoutItems().map((item) => item.widthPx)).toEqual([560, 560]);
  });

  it('re-anchors fit-mode widths so the smallest pane sits at the minimum', () => {
    setPaneLayoutItemsForTest([
      { id: 'a', paneId: 'a', kind: 'thread', widthPx: 1120 },
      { id: 'b', paneId: 'b', kind: 'thread', widthPx: 2240 },
    ]);

    setPaneHostWidth(5000);
    minAnchorPaneLayoutWidths(560);

    expect(getPaneLayoutItems().map((item) => item.widthPx)).toEqual([560, 1120]);

    // Already anchored: no-op.
    minAnchorPaneLayoutWidths(560);
    expect(getPaneLayoutItems().map((item) => item.widthPx)).toEqual([560, 1120]);
  });

  it('re-anchors a layout that fits the host exactly (dividers take no width)', () => {
    setPaneLayoutItemsForTest([
      { id: 'a', paneId: 'a', kind: 'thread', widthPx: 1120 },
      { id: 'b', paneId: 'b', kind: 'thread', widthPx: 2240 },
    ]);

    // Dividers are zero-width overlays, so an exactly-fitting total is
    // fit mode — reserving per-pane strip width here would misread it
    // as overflow and skip the anchor.
    setPaneHostWidth(3360);
    minAnchorPaneLayoutWidths(560);

    expect(getPaneLayoutItems().map((item) => item.widthPx)).toEqual([560, 1120]);
  });

  it('never re-anchors an overflowing or unmeasured host', () => {
    setPaneLayoutItemsForTest([
      { id: 'a', paneId: 'a', kind: 'thread', widthPx: 1120 },
      { id: 'b', paneId: 'b', kind: 'thread', widthPx: 2240 },
    ]);

    // Host never measured: overflow state unknowable, leave widths alone.
    minAnchorPaneLayoutWidths(560);
    expect(getPaneLayoutItems().map((item) => item.widthPx)).toEqual([1120, 2240]);

    // Total 3360 vs a 1200px host: the widths ARE the scroll extent the
    // user built; anchoring here would silently rescale it.
    setPaneHostWidth(1200);
    minAnchorPaneLayoutWidths(560);
    expect(getPaneLayoutItems().map((item) => item.widthPx)).toEqual([1120, 2240]);

    // Wide enough host: anchoring applies again.
    setPaneHostWidth(5000);
    minAnchorPaneLayoutWidths(560);
    expect(getPaneLayoutItems().map((item) => item.widthPx)).toEqual([560, 1120]);
  });
});
