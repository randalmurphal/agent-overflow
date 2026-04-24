import { scrollDeltaForMeasuredRowChange } from '../../utils/scrollAnchor';

type TimelineMeasurementOptions = {
  estimatedRowHeight: number;
  getRowHeight: (key: string) => number | undefined;
  getScrollContainer: () => HTMLDivElement | undefined;
  getUserPinnedToBottom: () => boolean;
  onRowHeightChanged: () => void;
  setRowHeight: (key: string, height: number) => void;
  setScrollContainer: (node: HTMLDivElement | undefined) => void;
  syncScrollState: () => void;
};

export function createTimelineMeasurementActions(options: TimelineMeasurementOptions) {
  function measureScrollContainer(node: HTMLElement) {
    options.setScrollContainer(node as HTMLDivElement);
    options.syncScrollState();

    const observer = new ResizeObserver(options.syncScrollState);
    observer.observe(node);

    return {
      destroy() {
        observer.disconnect();
        if (options.getScrollContainer() === node) {
          options.setScrollContainer(undefined);
        }
      },
    };
  }

  function measureTimelineRow(node: HTMLElement, key: string) {
    let currentKey = key;
    let previousHeight = previousKnownHeight(currentKey, options);

    const update = () => {
      const nextHeight = Math.ceil(node.getBoundingClientRect().height);
      if (nextHeight <= 0 || nextHeight === previousHeight) return;

      const scrollContainer = options.getScrollContainer();
      if (scrollContainer && previousHeight > 0 && !options.getUserPinnedToBottom()) {
        const rowRect = node.getBoundingClientRect();
        const viewportRect = scrollContainer.getBoundingClientRect();
        const scrollDelta = scrollDeltaForMeasuredRowChange({
          previousHeight,
          nextHeight,
          rowBottom: rowRect.bottom,
          viewportTop: viewportRect.top,
          userPinnedToBottom: false,
        });
        if (scrollDelta !== 0) {
          scrollContainer.scrollTop += scrollDelta;
          options.syncScrollState();
        }
      }

      previousHeight = nextHeight;
      options.setRowHeight(currentKey, nextHeight);
      options.onRowHeightChanged();
    };

    update();

    const observer = new ResizeObserver(update);
    observer.observe(node);

    return {
      update(nextKey: string) {
        if (nextKey === currentKey) return;

        currentKey = nextKey;
        previousHeight = previousKnownHeight(currentKey, options);
        update();
      },
      destroy() {
        observer.disconnect();
      },
    };
  }

  return {
    measureScrollContainer,
    measureTimelineRow,
  };
}

function previousKnownHeight(key: string, options: TimelineMeasurementOptions): number {
  return options.getRowHeight(key) ?? options.estimatedRowHeight;
}
