import { scrollDeltaForMeasuredRowChange } from '../../utils/scrollAnchor';

type TimelineMeasurementOptions = {
  estimatedRowHeight: number;
  getRowHeight: (key: string) => number | undefined;
  getScrollContainer: () => HTMLDivElement | undefined;
  getIsSticky: () => boolean;
  onRowHeightChanged: () => void;
  setRowHeight: (key: string, height: number) => void;
  setScrollContainer: (node: HTMLDivElement | undefined) => void;
  syncViewportState: () => void;
};

type TimelineRowMeasurement = {
  key: string;
  estimatedHeight: number;
};

export function createTimelineMeasurementActions(options: TimelineMeasurementOptions) {
  function measureScrollContainer(node: HTMLElement) {
    options.setScrollContainer(node as HTMLDivElement);
    options.syncViewportState();

    const observer = new ResizeObserver(options.syncViewportState);
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

  function measureTimelineRow(node: HTMLElement, measurement: TimelineRowMeasurement) {
    let current = measurement;
    let previousHeight = previousKnownHeight(current, options);

    const update = () => {
      const nextHeight = Math.ceil(node.getBoundingClientRect().height);
      if (nextHeight <= 0 || nextHeight === previousHeight) return;

      const scrollContainer = options.getScrollContainer();
      if (scrollContainer && previousHeight > 0 && !options.getIsSticky()) {
        const rowRect = node.getBoundingClientRect();
        const viewportRect = scrollContainer.getBoundingClientRect();
        const scrollDelta = scrollDeltaForMeasuredRowChange({
          previousHeight,
          nextHeight,
          rowBottom: rowRect.bottom,
          viewportTop: viewportRect.top,
        });
        if (scrollDelta !== 0) {
          scrollContainer.scrollTop += scrollDelta;
          options.syncViewportState();
        }
      }

      previousHeight = nextHeight;
      options.setRowHeight(current.key, nextHeight);
      options.onRowHeightChanged();
    };

    update();

    const observer = new ResizeObserver(update);
    observer.observe(node);

    return {
      update(next: TimelineRowMeasurement) {
        if (next.key === current.key && next.estimatedHeight === current.estimatedHeight) return;

        current = next;
        previousHeight = previousKnownHeight(current, options);
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

function previousKnownHeight(
  measurement: TimelineRowMeasurement,
  options: TimelineMeasurementOptions,
): number {
  return options.getRowHeight(measurement.key) ?? measurement.estimatedHeight ?? options.estimatedRowHeight;
}
