import { cleanup, render } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import PaneDivider from './PaneDivider.svelte';
import { resetLayoutMetricsForTest, setPaneHostWidth, setPaneWidth } from '../../stores/layoutMetrics.svelte';
import {
  getPaneLayoutItems,
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
} from '../../stores/paneLayout.svelte';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { resetSettingsForTest } from '../../stores/settings.svelte';

// Compact density minimum (the default after resetSettingsForTest).
const MIN = 560;

// jsdom has no pointer capture; the component optional-chains
// hasPointerCapture, so no-op shims are enough.
if (!Element.prototype.setPointerCapture) {
  Element.prototype.setPointerCapture = () => {};
  Element.prototype.releasePointerCapture = () => {};
  Element.prototype.hasPointerCapture = () => false;
}

interface HostSpec {
  scrollWidth: number;
  clientWidth: number;
  rectLeft?: number;
}

// A stand-in pane-strip host: scroll metrics and a clamping scrollLeft
// setter that mirrors how a real browser refuses to scroll past
// scrollWidth - clientWidth.
function makeHost({ scrollWidth, clientWidth, rectLeft = 0 }: HostSpec): HTMLElement {
  const host = document.createElement('div');
  const maxScroll = Math.max(0, scrollWidth - clientWidth);
  let scrollLeft = 0;
  Object.defineProperty(host, 'scrollWidth', { configurable: true, get: () => scrollWidth });
  Object.defineProperty(host, 'clientWidth', { configurable: true, get: () => clientWidth });
  Object.defineProperty(host, 'scrollLeft', {
    configurable: true,
    get: () => scrollLeft,
    set: (value: number) => {
      scrollLeft = Math.min(maxScroll, Math.max(0, value));
    },
  });
  Object.defineProperty(host, 'getBoundingClientRect', {
    configurable: true,
    value: () => ({
      left: rectLeft,
      right: rectLeft + clientWidth,
      top: 0,
      bottom: 400,
      width: clientWidth,
      height: 400,
      x: rectLeft,
      y: 0,
      toJSON: () => ({}),
    }),
  });
  return host;
}

interface PointerInit {
  clientX?: number;
  button?: number;
  altKey?: boolean;
  pointerId?: number;
}

// jsdom lacks PointerEvent; the component only reads MouseEvent fields
// plus pointerId, so a MouseEvent with pointerId grafted on suffices.
function firePointer(el: HTMLElement, type: string, init: PointerInit = {}): void {
  const event = new MouseEvent(type, {
    bubbles: true,
    cancelable: true,
    button: init.button ?? 0,
    clientX: init.clientX ?? 0,
    altKey: init.altKey ?? false,
  });
  Object.defineProperty(event, 'pointerId', { configurable: true, value: init.pointerId ?? 1 });
  el.dispatchEvent(event);
}

function dblclick(el: HTMLElement): void {
  el.dispatchEvent(new MouseEvent('dblclick', { bubbles: true, cancelable: true }));
}

interface DividerProps {
  leftPaneId: string;
  rightPaneId?: string;
  leftPaneWidthPx?: number;
  host: HTMLElement;
  onDragEnd?: () => void;
}

function renderDivider({
  leftPaneId,
  rightPaneId,
  leftPaneWidthPx = 800,
  host,
  onDragEnd,
}: DividerProps): HTMLElement {
  const rendered = render(PaneDivider, {
    props: { leftPaneId, rightPaneId, leftPaneWidthPx, getHostEl: () => host, onDragEnd },
  });
  return rendered.getByTestId(rightPaneId ? 'pane-divider' : 'pane-end-handle') as HTMLElement;
}

function layoutWidths(): number[] {
  return getPaneLayoutItems().map((item) => item.widthPx);
}

describe('PaneDivider', () => {
  let pendingFrames: Map<number, FrameRequestCallback>;

  function flushFrames(): void {
    const frames = Array.from(pendingFrames.values());
    pendingFrames.clear();
    for (const frame of frames) frame(0);
  }

  beforeEach(() => {
    resetLayoutMetricsForTest();
    resetPanesForTest();
    resetPaneLayoutForTest();
    resetSettingsForTest();
    setPaneLayoutItemsForTest([
      { id: 'a', paneId: 'a', kind: 'thread', widthPx: 800 },
      { id: 'b', paneId: 'b', kind: 'thread', widthPx: 800 },
    ]);
    setPaneWidth('a', 800);
    setPaneWidth('b', 800);

    pendingFrames = new Map();
    let nextFrameId = 1;
    vi.spyOn(window, 'requestAnimationFrame').mockImplementation((callback) => {
      const frameId = nextFrameId;
      nextFrameId += 1;
      pendingFrames.set(frameId, callback);
      return frameId;
    });
    vi.spyOn(window, 'cancelAnimationFrame').mockImplementation((frameId) => {
      pendingFrames.delete(frameId);
    });
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    resetLayoutMetricsForTest();
    resetPanesForTest();
    resetPaneLayoutForTest();
    resetSettingsForTest();
    document.body.style.cursor = '';
    document.body.style.userSelect = '';
  });

  it('fit-mode drag trades width with the right neighbor and restores body styles', () => {
    const host = makeHost({ scrollWidth: 1600, clientWidth: 1600 });
    const divider = renderDivider({ leftPaneId: 'a', rightPaneId: 'b', host });

    firePointer(divider, 'pointerdown', { clientX: 500 });
    expect(document.body.style.cursor).toBe('col-resize');
    expect(document.body.style.userSelect).toBe('none');

    firePointer(divider, 'pointermove', { clientX: 600 });
    firePointer(divider, 'pointerup', { clientX: 600 });

    expect(layoutWidths()).toEqual([900, 700]);
    expect(document.body.style.cursor).toBe('');
    expect(document.body.style.userSelect).toBe('');
  });

  it('snapshots measured pane widths at drag start, not stored layout widths', () => {
    // The stored layout says 800/800 but the DOM measured 1000/900
    // (e.g. the strip stretched to fill a wider window): the drag must
    // resolve from what the user actually sees.
    setPaneWidth('a', 1000);
    setPaneWidth('b', 900);
    const host = makeHost({ scrollWidth: 1900, clientWidth: 1900 });
    const divider = renderDivider({ leftPaneId: 'a', rightPaneId: 'b', host });

    firePointer(divider, 'pointerdown', { clientX: 500 });
    firePointer(divider, 'pointermove', { clientX: 600 });
    firePointer(divider, 'pointerup', { clientX: 600 });

    expect(layoutWidths()).toEqual([1100, 800]);
  });

  it('overflow drag grows the left pane without resizing the neighbor', () => {
    const host = makeHost({ scrollWidth: 1600, clientWidth: 1000 });
    const divider = renderDivider({ leftPaneId: 'a', rightPaneId: 'b', host });

    firePointer(divider, 'pointerdown', { clientX: 500 });
    firePointer(divider, 'pointermove', { clientX: 600 });
    firePointer(divider, 'pointerup', { clientX: 600 });

    expect(layoutWidths()).toEqual([900, 800]);
  });

  it('Alt+drag forces a zero-sum trade even while overflowing', () => {
    const host = makeHost({ scrollWidth: 1600, clientWidth: 1000 });
    const divider = renderDivider({ leftPaneId: 'a', rightPaneId: 'b', host });

    firePointer(divider, 'pointerdown', { clientX: 500, altKey: true });
    firePointer(divider, 'pointermove', { clientX: 600 });
    firePointer(divider, 'pointerup', { clientX: 600 });

    expect(layoutWidths()).toEqual([900, 700]);
  });

  it('ignores non-primary buttons entirely', () => {
    const host = makeHost({ scrollWidth: 1600, clientWidth: 1600 });
    const divider = renderDivider({ leftPaneId: 'a', rightPaneId: 'b', host });

    firePointer(divider, 'pointerdown', { clientX: 500, button: 2 });
    expect(document.body.style.cursor).toBe('');
    firePointer(divider, 'pointermove', { clientX: 600 });
    firePointer(divider, 'pointerup', { clientX: 600 });

    expect(layoutWidths()).toEqual([800, 800]);
  });

  it('end handle drag resizes the last pane', () => {
    const host = makeHost({ scrollWidth: 1600, clientWidth: 1000 });
    const handle = renderDivider({ leftPaneId: 'b', host });

    firePointer(handle, 'pointerdown', { clientX: 500 });
    firePointer(handle, 'pointermove', { clientX: 600 });
    firePointer(handle, 'pointerup', { clientX: 600 });

    expect(layoutWidths()).toEqual([800, 900]);
  });

  it('min-anchors the layout at drag end only when the host is measured and fits', () => {
    const host = makeHost({ scrollWidth: 1600, clientWidth: 1600 });
    const divider = renderDivider({ leftPaneId: 'a', rightPaneId: 'b', host });

    // Unmeasured host: the raw drag result stands.
    firePointer(divider, 'pointerdown', { clientX: 500 });
    firePointer(divider, 'pointermove', { clientX: 600 });
    firePointer(divider, 'pointerup', { clientX: 600 });
    expect(layoutWidths()).toEqual([900, 700]);

    // Measured host the layout fits in: drag end re-anchors so the
    // smallest pane sits at the density minimum (1600 total ≤ 1608
    // host minus 2×4px divider strips). The measured widths track the
    // first drag's result, as the pane ResizeObservers would republish.
    setPaneWidth('a', 900);
    setPaneWidth('b', 700);
    setPaneHostWidth(1608);
    firePointer(divider, 'pointerdown', { clientX: 500 });
    firePointer(divider, 'pointermove', { clientX: 500 });
    firePointer(divider, 'pointerup', { clientX: 500 });
    // 900/700 scaled by 560/700.
    expect(layoutWidths()).toEqual([720, 560]);
  });

  it('double-click equalizes to the density minimum, but not after a real drag', () => {
    const host = makeHost({ scrollWidth: 1600, clientWidth: 1600 });
    const divider = renderDivider({ leftPaneId: 'a', rightPaneId: 'b', host });

    dblclick(divider);
    expect(layoutWidths()).toEqual([MIN, MIN]);

    setPaneLayoutItemsForTest([
      { id: 'a', paneId: 'a', kind: 'thread', widthPx: 800 },
      { id: 'b', paneId: 'b', kind: 'thread', widthPx: 800 },
    ]);
    // A drag with real travel: the trailing dblclick is two fine-tune
    // drags misread by the browser, not a reset request.
    firePointer(divider, 'pointerdown', { clientX: 500 });
    firePointer(divider, 'pointermove', { clientX: 550 });
    firePointer(divider, 'pointerup', { clientX: 550 });
    expect(layoutWidths()).toEqual([850, 750]);

    dblclick(divider);
    expect(layoutWidths()).toEqual([850, 750]);
  });

  it('keeps the reset suppressed when only the FIRST of two rapid drags travelled', () => {
    const host = makeHost({ scrollWidth: 1600, clientWidth: 1600 });
    const divider = renderDivider({ leftPaneId: 'a', rightPaneId: 'b', host });

    // Drag 1 travels a real distance.
    firePointer(divider, 'pointerdown', { clientX: 500 });
    firePointer(divider, 'pointermove', { clientX: 560 });
    firePointer(divider, 'pointerup', { clientX: 560 });
    const afterDrags = layoutWidths();
    expect(afterDrags).toEqual([860, 740]);
    // The real app's ResizeObservers republish the measured widths after a
    // resize; mirror that so drag 2's start snapshot isn't stale.
    setPaneWidth('a', 860);
    setPaneWidth('b', 740);

    // Drag 2 is effectively a click — no travel — so the browser fires a
    // dblclick. Suppression must fire off the PREVIOUS gesture's travel
    // (prevGestureTravelPx), or the reset would wrongly nuke the layout.
    firePointer(divider, 'pointerdown', { clientX: 560 });
    firePointer(divider, 'pointerup', { clientX: 560 });
    dblclick(divider);

    expect(layoutWidths()).toEqual(afterDrags);
  });

  it('edge auto-scroll keeps feeding the resize while the pointer sits still', () => {
    const host = makeHost({ scrollWidth: 1600, clientWidth: 1000 });
    const divider = renderDivider({ leftPaneId: 'a', rightPaneId: 'b', host });

    firePointer(divider, 'pointerdown', { clientX: 500 });
    // Pointer parked at the host's right edge: max step (18px/frame).
    firePointer(divider, 'pointermove', { clientX: 1000 });

    flushFrames();
    expect(host.scrollLeft).toBe(18);
    expect(layoutWidths()).toEqual([800 + 500 + 18, 800]);

    flushFrames();
    expect(host.scrollLeft).toBe(36);
    expect(layoutWidths()).toEqual([800 + 500 + 36, 800]);

    firePointer(divider, 'pointerup', { clientX: 1000 });
    expect(layoutWidths()).toEqual([1336, 800]);
    // The frame loop is cancelled with the drag.
    expect(pendingFrames.size).toBe(0);
  });

  it('external scroll changes do not compound into the resize delta', () => {
    // Regression: the delta used to be derived by diffing raw
    // host.scrollLeft against its drag-start value, so any scroll the
    // drag did NOT itself cause — a browser clamp when a shrinking pane
    // shrinks scrollWidth, momentum scroll — fed straight into the width.
    // The fix accounts only for scroll the gesture performs
    // (autoScrolledPx), so the resize tracks pointer travel alone.
    const host = makeHost({ scrollWidth: 1600, clientWidth: 1000 });
    const divider = renderDivider({ leftPaneId: 'a', rightPaneId: 'b', host });

    // Pointer parked well away from either edge → no gesture auto-scroll.
    firePointer(divider, 'pointerdown', { clientX: 500 });
    firePointer(divider, 'pointermove', { clientX: 520 });
    // Something else scrolls the strip mid-drag.
    host.scrollLeft = 200;

    flushFrames();
    flushFrames();

    // 20px of pointer travel, overflow mode → left pane grows by 20, the
    // 200px external scroll is ignored (pre-fix it would read [1020, 800]).
    expect(layoutWidths()).toEqual([820, 800]);
  });

  it('pointercancel commits the in-progress resize and restores body styles', () => {
    const onDragEnd = vi.fn();
    const host = makeHost({ scrollWidth: 1600, clientWidth: 1600 });
    const divider = renderDivider({ leftPaneId: 'a', rightPaneId: 'b', host, onDragEnd });

    firePointer(divider, 'pointerdown', { clientX: 500 });
    expect(document.body.style.cursor).toBe('col-resize');
    firePointer(divider, 'pointermove', { clientX: 540 });
    firePointer(divider, 'pointercancel', { clientX: 540 });

    // Cancel is an end-of-gesture, not a revert: the final width (a 40px
    // fit trade) is committed exactly as pointerup would.
    expect(layoutWidths()).toEqual([840, 760]);
    expect(onDragEnd).toHaveBeenCalledTimes(1);
    expect(document.body.style.cursor).toBe('');
    expect(document.body.style.userSelect).toBe('');
    expect(pendingFrames.size).toBe(0);
  });

  it('notifies onDragEnd after a completed drag and after a double-click reset', () => {
    const onDragEnd = vi.fn();
    const host = makeHost({ scrollWidth: 1600, clientWidth: 1600 });
    const divider = renderDivider({ leftPaneId: 'a', rightPaneId: 'b', host, onDragEnd });

    firePointer(divider, 'pointerdown', { clientX: 500 });
    firePointer(divider, 'pointermove', { clientX: 502 });
    firePointer(divider, 'pointerup', { clientX: 502 });
    expect(onDragEnd).toHaveBeenCalledTimes(1);

    dblclick(divider);
    expect(onDragEnd).toHaveBeenCalledTimes(2);
  });
});
