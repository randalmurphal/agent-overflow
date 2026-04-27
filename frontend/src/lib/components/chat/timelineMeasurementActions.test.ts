import { describe, expect, it } from 'vitest';
import { createTimelineMeasurementActions } from './timelineMeasurementActions';
import { installControllableResizeObserver, rect } from '../../../test/helpers/scrollDom';

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
      getIsSticky: () => false,
      onRowHeightChanged: () => {
        revisionCount += 1;
      },
      setRowHeight: (key, height) => {
        rowHeights.set(key, height);
      },
      setScrollContainer: () => {},
      syncViewportState: () => {
        syncCount += 1;
      },
    });

    const action = actions.measureTimelineRow(row, { key: 'row-1', estimatedHeight: 140 });

    expect(scrollContainer.scrollTop).toBe(620);
    expect(rowHeights.get('row-1')).toBe(260);
    expect(revisionCount).toBe(1);
    expect(syncCount).toBe(1);

    action.destroy();
  });

  it('uses the mounted row estimate as the previous height before first measurement', () => {
    const rowHeights = new Map<string, number>();
    const scrollContainer = {
      scrollTop: 500,
      getBoundingClientRect: () => rect({ top: 100 }),
    } as HTMLDivElement;
    const row = {
      getBoundingClientRect: () => rect({ height: 72, bottom: 140 }),
    } as HTMLElement;

    const actions = createTimelineMeasurementActions({
      estimatedRowHeight: 140,
      getRowHeight: (key) => rowHeights.get(key),
      getScrollContainer: () => scrollContainer,
      getIsSticky: () => false,
      onRowHeightChanged: () => {},
      setRowHeight: (key, height) => {
        rowHeights.set(key, height);
      },
      setScrollContainer: () => {},
      syncViewportState: () => {},
    });

    const action = actions.measureTimelineRow(row, { key: 'wait-row', estimatedHeight: 32 });

    expect(scrollContainer.scrollTop).toBe(540);
    expect(rowHeights.get('wait-row')).toBe(72);

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
      getIsSticky: () => true,
      onRowHeightChanged: () => {},
      setRowHeight: (key, height) => {
        rowHeights.set(key, height);
      },
      setScrollContainer: () => {},
      syncViewportState: () => {},
    });

    const action = actions.measureTimelineRow(row, { key: 'row-1', estimatedHeight: 140 });

    expect(scrollContainer.scrollTop).toBe(500);
    expect(rowRectReadCount).toBe(1);
    expect(rowHeights.get('row-1')).toBe(260);

    action.destroy();
  });

  it('syncs viewport state on scroll container resize without touching bottom intent', () => {
    let synced = 0;
    const resize = installControllableResizeObserver();
    try {
      const actions = createTimelineMeasurementActions({
        estimatedRowHeight: 140,
        getRowHeight: () => undefined,
        getScrollContainer: () => undefined,
        getIsSticky: () => {
          throw new Error('container resize must not inspect bottom intent');
        },
        onRowHeightChanged: () => {},
        setRowHeight: () => {},
        setScrollContainer: () => {},
        syncViewportState: () => {
          synced += 1;
        },
      });

      const action = actions.measureScrollContainer({} as HTMLElement);
      // Trigger the registered ResizeObserver callback once. We expect
      // `synced` to be exactly 2 (one from measureScrollContainer's
      // initial sync, one from the trigger). If multiple callbacks were
      // registered, synced would jump higher.
      resize.trigger();

      expect(synced).toBe(2);
      action.destroy();
    } finally {
      resize.restore();
    }
  });
});
