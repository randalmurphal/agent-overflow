import { afterEach, expect, it, vi } from 'vitest';
import { createSpringChase, __resetSpringFrameBatcherForTest } from './spring';
import { documentScrollGrid } from './grid';
import { SpringMotion } from './motion';

const originalZoom = document.documentElement.style.zoom;
afterEach(() => {
  __resetSpringFrameBatcherForTest();
  vi.restoreAllMocks();
  document.documentElement.style.zoom = originalZoom;
  window.dispatchEvent(new Event('resize'));
});

it.each([1, 1.25, 1.5, 2, 0.8].flatMap((zoom) =>
  [30, 60, 90, 120, 144, 165, 220, 240, 360, 480].map((hz) => ({ zoom, hz })),
))('a real scroller at $zoom scale / $hz Hz samples the continuous path within its measured grid', ({ zoom, hz }) => {
  document.documentElement.style.zoom = String(zoom);
  window.dispatchEvent(new Event('resize'));
  const scroller = document.createElement('div');
  scroller.style.cssText = 'width:100px;height:100px;overflow:auto;overflow-anchor:none';
  const content = document.createElement('div');
  content.style.height = '260px';
  scroller.appendChild(content);
  document.body.appendChild(scroller);
  let now = 0;
  let frameIndex = 0;
  const frames = new Map<number, FrameRequestCallback>();
  let nextFrameId = 0;
  vi.spyOn(performance, 'now').mockImplementation(() => now);
  vi.spyOn(window, 'requestAnimationFrame').mockImplementation((cb) => { frames.set(++nextFrameId, cb); return nextFrameId; });
  vi.spyOn(window, 'cancelAnimationFrame').mockImplementation((id) => { frames.delete(id); });
  __resetSpringFrameBatcherForTest();
  const reference = new SpringMotion();
  let referenceTop = 96;
  const continuousGrid = { quantum: 1e-8, writeOffset: 0, readbackError: 0 };
  const accept = (_: number, position: number) => position;
  const grid = documentScrollGrid(document);
  const target = 160;
  scroller.scrollTop = 96;
  referenceTop = scroller.scrollTop;
  const spring = createSpringChase({
    getScrollEl: () => scroller, isPaused: () => false, isAtBottom: () => true,
    isEscaped: () => false, selectionActive: () => false,
    targetScrollTop: () => target, currentScrollTop: () => scroller.scrollTop,
    arrival: {
      matches: () => false, record: () => {}, shouldWriteExact: (top) => scroller.scrollTop !== top,
      writeExact: (_, top) => { scroller.scrollTop = top; }, clear: () => {}, invalidateStale: () => {},
    },
    writeScrollTop: (_, top) => {
      scroller.scrollTop = top;
      return scroller.scrollTop;
    },
    liveContentActive: () => true, prefersReducedMotion: () => false,
    scrollGrid: () => grid, forceNextSpringTickTrace: () => {},
    scrollTopUnexplained: () => false,
    reportWriteRefusal: () => { throw new Error('A calibrated scrollable element refused motion'); },
  });
  try {
    spring.start();
    for (; frameIndex < hz * 5 && scroller.scrollTop !== target; frameIndex++) {
      now += 1000 / hz;
      const callbacks = [...frames.values()];
      frames.clear();
      for (const callback of callbacks) callback(now);
      referenceTop = reference.step(referenceTop, target, frameIndex === 0 ? 1 : 60 / hz, continuousGrid, accept);
      expect(Math.abs(scroller.scrollTop - referenceTop), `frame ${frameIndex}`).toBeLessThanOrEqual(grid.quantum / 2 + grid.readbackError + 1e-3);
    }
    expect(scroller.scrollTop).toBe(target);

  } finally {
    spring.cancel();
    scroller.remove();
  }
});
