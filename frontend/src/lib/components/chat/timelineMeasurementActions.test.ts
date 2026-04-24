import { describe, expect, it } from 'vitest';
import { createTimelineMeasurementActions } from './timelineMeasurementActions';

function rect(partial: Partial<DOMRect>): DOMRect {
  return {
    bottom: 0,
    height: 0,
    left: 0,
    right: 0,
    top: 0,
    width: 0,
    x: 0,
    y: 0,
    toJSON: () => ({}),
    ...partial,
  };
}

describe('createTimelineMeasurementActions', () => {
  it('anchors the viewport when an initially estimated row above the viewport is first measured', () => {
    const rowHeights = new Map<string, number>();
    const scrollContainer = {
      scrollTop: 500,
      getBoundingClientRect: () => rect({ top: 100 }),
    } as HTMLDivElement;
    const row = {
      getBoundingClientRect: () => rect({ height: 260, bottom: 140 }),
    } as HTMLElement;
    let revisionCount = 0;
    let syncCount = 0;

    const actions = createTimelineMeasurementActions({
      estimatedRowHeight: 140,
      getRowHeight: (key) => rowHeights.get(key),
      getScrollContainer: () => scrollContainer,
      getUserPinnedToBottom: () => false,
      onRowHeightChanged: () => {
        revisionCount += 1;
      },
      setRowHeight: (key, height) => {
        rowHeights.set(key, height);
      },
      setScrollContainer: () => {},
      syncScrollState: () => {
        syncCount += 1;
      },
    });

    const action = actions.measureTimelineRow(row, 'row-1');

    expect(scrollContainer.scrollTop).toBe(620);
    expect(rowHeights.get('row-1')).toBe(260);
    expect(revisionCount).toBe(1);
    expect(syncCount).toBe(1);

    action.destroy();
  });

  it('does not do anchor geometry work while pinned to the bottom', () => {
    const rowHeights = new Map<string, number>();
    let rowRectReadCount = 0;
    const scrollContainer = {
      scrollTop: 500,
      getBoundingClientRect: () => {
        throw new Error('viewport geometry should not be read');
      },
    } as unknown as HTMLDivElement;
    const row = {
      getBoundingClientRect: () => {
        rowRectReadCount += 1;
        return rect({ height: 260, bottom: 140 });
      },
    } as HTMLElement;

    const actions = createTimelineMeasurementActions({
      estimatedRowHeight: 140,
      getRowHeight: (key) => rowHeights.get(key),
      getScrollContainer: () => scrollContainer,
      getUserPinnedToBottom: () => true,
      onRowHeightChanged: () => {},
      setRowHeight: (key, height) => {
        rowHeights.set(key, height);
      },
      setScrollContainer: () => {},
      syncScrollState: () => {},
    });

    const action = actions.measureTimelineRow(row, 'row-1');

    expect(scrollContainer.scrollTop).toBe(500);
    expect(rowRectReadCount).toBe(1);
    expect(rowHeights.get('row-1')).toBe(260);

    action.destroy();
  });
});
