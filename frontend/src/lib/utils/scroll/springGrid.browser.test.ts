import { afterEach, expect, it, vi } from 'vitest';
import { createSpringChase, __resetSpringFrameBatcherForTest } from './spring';
import { documentScrollGrid } from './grid';
import { quantizedFloorStep } from './cadence';

const originalZoom = document.documentElement.style.zoom;
afterEach(() => {
  __resetSpringFrameBatcherForTest();
  vi.restoreAllMocks();
  document.documentElement.style.zoom = originalZoom;
  window.dispatchEvent(new Event('resize'));
});

it.each([1, 1.25, 1.5, 2, 0.8].flatMap((zoom) =>
  [30, 60, 120, 165, 220, 240].map((hz) => ({ zoom, hz })),
))('a real scroller at $zoom scale / $hz Hz lands with an even cradle', ({ zoom, hz }) => {
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
  const events: number[] = [];
  const grid = documentScrollGrid(document);
  const target = 160;
  scroller.scrollTop = 96;
  const spring = createSpringChase({
    getScrollEl: () => scroller, isPaused: () => false, isAtBottom: () => true,
    isEscaped: () => false, selectionActive: () => false,
    targetScrollTop: () => target, currentScrollTop: () => scroller.scrollTop,
    scrollTopIsAtTarget: (top) => Math.abs(top - scroller.scrollTop) <= 1,
    arrival: {
      matches: () => false, record: () => {}, shouldWriteExact: (top) => scroller.scrollTop !== top,
      writeExact: (_, top) => { scroller.scrollTop = top; }, clear: () => {}, invalidateStale: () => {},
    },
    writeScrollTop: (_, top) => {
      const before = scroller.scrollTop;
      scroller.scrollTop = top;
      if (scroller.scrollTop !== before) events.push(frameIndex);
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
    }
    expect(scroller.scrollTop).toBe(target);
    const k = Math.max(1, Math.round(1 / quantizedFloorStep(1 / grid.quantum, 60 / hz)));
    const tail = events.slice(-5);
    expect(tail.slice(1).map((event, index) => event - tail[index])).toEqual([k, k, 2 * k, 3 * k]);
  } finally {
    spring.cancel();
    scroller.remove();
  }
});
