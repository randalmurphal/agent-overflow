export type ResizeAxis = 'x' | 'y';

export interface ResizeGestureOptions {
  axis: ResizeAxis;
  cursor: string;
  currentSize: number;
  minSize: number;
  maxSize: number;
  direction: 1 | -1;
  onResizeLive(size: number): void;
  onResizeEnd(size: number): void;
  acquireLease?: () => (() => void) | null;
}

function pointerFor(axis: ResizeAxis, event: PointerEvent): number {
  return axis === 'x' ? event.clientX : event.clientY;
}

function restoreBodyStyles(): void {
  document.body.style.cursor = '';
  document.body.style.userSelect = '';
}

export function createResizeGesture(readOptions: () => ResizeGestureOptions) {
  let dragging = $state(false);
  let startPointer = 0;
  let startSize = 0;
  let lockedMax = Number.POSITIVE_INFINITY;
  let activeSize = 0;
  let activePointerId: number | null = null;
  let activeTarget: HTMLElement | null = null;
  let activeOptions: ResizeGestureOptions | null = null;
  let releaseLease: (() => void) | null = null;
  let liveFrame: number | null = null;
  let liveEmittedSize = 0;

  function clamp(value: number, min: number, max: number): number {
    return Math.max(min, Math.min(max, value));
  }

  function onPointerDown(event: PointerEvent): void {
    const options = readOptions();
    event.preventDefault();
    window.getSelection()?.removeAllRanges();
    dragging = true;
    activeOptions = options;
    startPointer = pointerFor(options.axis, event);
    startSize = options.currentSize;
    activeSize = options.currentSize;
    liveEmittedSize = options.currentSize;
    lockedMax = options.maxSize;
    activePointerId = event.pointerId;
    activeTarget = event.currentTarget as HTMLElement;
    activeTarget.setPointerCapture(event.pointerId);
    document.body.style.cursor = options.cursor;
    document.body.style.userSelect = 'none';
    releaseLease = options.acquireLease?.() ?? null;
  }

  function flushLiveResize(options: ResizeGestureOptions): void {
    if (liveFrame !== null) {
      cancelAnimationFrame(liveFrame);
      liveFrame = null;
    }
    if (activeSize === liveEmittedSize) return;
    liveEmittedSize = activeSize;
    options.onResizeLive(activeSize);
  }

  function scheduleLiveResize(options: ResizeGestureOptions): void {
    if (liveFrame !== null) return;
    liveFrame = requestAnimationFrame(() => {
      liveFrame = null;
      if (!dragging || activeOptions !== options) return;
      flushLiveResize(options);
    });
  }

  function onPointerMove(event: PointerEvent): void {
    const options = activeOptions;
    if (!dragging || !options) return;
    const delta = pointerFor(options.axis, event) - startPointer;
    const next = clamp(
      startSize + delta * options.direction,
      options.minSize,
      lockedMax,
    );
    if (next !== activeSize) {
      activeSize = next;
      scheduleLiveResize(options);
    }
  }

  function endDrag(event?: PointerEvent): void {
    const options = activeOptions;
    if (!dragging || !options) return;
    dragging = false;
    if (event) {
      (event.currentTarget as HTMLElement).releasePointerCapture(event.pointerId);
    } else if (activeTarget && activePointerId !== null) {
      activeTarget.releasePointerCapture(activePointerId);
    }
    flushLiveResize(options);
    activePointerId = null;
    activeTarget = null;
    restoreBodyStyles();
    releaseLease?.();
    releaseLease = null;
    options.onResizeEnd(activeSize);
    activeOptions = null;
  }

  function destroy(): void {
    if (dragging) restoreBodyStyles();
    if (liveFrame !== null) {
      cancelAnimationFrame(liveFrame);
      liveFrame = null;
    }
    releaseLease?.();
    releaseLease = null;
    dragging = false;
    activeOptions = null;
    activePointerId = null;
    activeTarget = null;
  }

  return {
    get dragging() { return dragging; },
    onPointerDown,
    onPointerMove,
    endDrag,
    destroy,
  };
}
