const RENDERED_HEIGHT_MEASURE_RETRY_LIMIT = 4;
const DEFAULT_RECORD_OPTIONS = { allowFrameRetry: true };

type RenderedHeightRecorderOptions = {
  root: () => HTMLElement | undefined;
  innerSelector: string;
  cacheKey: () => string;
  writeHeight: (key: string, height: number) => void;
};

type RenderedHeightRecorder = {
  record: () => void;
  cancel: () => void;
};

export function createRenderedHeightRecorder({
  root,
  innerSelector,
  cacheKey,
  writeHeight,
}: RenderedHeightRecorderOptions): RenderedHeightRecorder {
  let pendingFrame: number | undefined;
  let resizeObserver: ResizeObserver | undefined;
  let observedInner: HTMLElement | undefined;
  let hasWrittenHeight = false;

  const cancelPendingFrame = (): void => {
    if (pendingFrame === undefined) return;
    if (typeof cancelAnimationFrame === 'function') {
      cancelAnimationFrame(pendingFrame);
    }
    pendingFrame = undefined;
  };

  const stopObserving = (): void => {
    resizeObserver?.disconnect();
    resizeObserver = undefined;
    observedInner = undefined;
  };

  const cancel = (): void => {
    cancelPendingFrame();
    stopObserving();
  };

  const scheduleRetry = (attempt: number): void => {
    if (
      attempt >= RENDERED_HEIGHT_MEASURE_RETRY_LIMIT ||
      typeof requestAnimationFrame !== 'function'
    ) {
      return;
    }
    cancelPendingFrame();
    pendingFrame = requestAnimationFrame(() => {
      pendingFrame = undefined;
      recordAttempt(attempt + 1);
    });
  };

  const observeInner = (inner: HTMLElement): void => {
    if (
      hasWrittenHeight ||
      observedInner === inner ||
      typeof ResizeObserver === 'undefined'
    ) {
      return;
    }
    stopObserving();
    observedInner = inner;
    resizeObserver = new ResizeObserver(() => {
      recordAttempt(0, { allowFrameRetry: false });
    });
    resizeObserver.observe(inner);
  };

  const recordAttempt = (
    attempt: number,
    { allowFrameRetry }: { allowFrameRetry: boolean } = DEFAULT_RECORD_OPTIONS,
  ): void => {
    if (hasWrittenHeight) return;

    const inner = root()?.querySelector<HTMLElement>(innerSelector);
    if (!inner) {
      scheduleRetry(attempt);
      return;
    }

    const height = inner.offsetHeight;
    if (height > 0) {
      hasWrittenHeight = true;
      cancel();
      writeHeight(cacheKey(), height);
      return;
    }

    observeInner(inner);
    if (allowFrameRetry) {
      scheduleRetry(attempt);
    }
  };

  return {
    record: () => recordAttempt(0),
    cancel,
  };
}
