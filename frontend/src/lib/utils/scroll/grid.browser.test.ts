import { afterEach, expect, it } from 'vitest';
import { documentScrollGrid } from './grid';

const originalZoom = document.documentElement.style.zoom;
afterEach(() => {
  document.documentElement.style.zoom = originalZoom;
  window.dispatchEvent(new Event('resize'));
});

it('calibrates the real engine across scale changes without moving an existing scroller', () => {
  const scroller = document.createElement('div');
  scroller.style.cssText = 'position:fixed;width:100px;height:100px;overflow:auto';
  const content = document.createElement('div');
  content.style.height = '1000px';
  scroller.appendChild(content);
  document.body.appendChild(scroller);
  try {
    // CSS zoom provides deterministic scaling within the browser test
    // document. Real tab zoom is also covered by the manual display matrix.
    for (const zoom of [1, 1.25, 1.5, 2, 0.8, 1]) {
      document.documentElement.style.zoom = String(zoom);
      window.dispatchEvent(new Event('resize'));
      scroller.scrollTop = 100;
      const before = scroller.scrollTop;
      const nodes = document.querySelectorAll('*').length;
      const grid = documentScrollGrid(document);
      expect(scroller.scrollTop).toBe(before);
      expect(document.querySelectorAll('*').length).toBe(nodes);
      expect(documentScrollGrid(document), 'unchanged scale reuses the measurement').toBe(grid);
      expect(grid.quantum).toBeCloseTo(1 / zoom, 5);
      for (const direction of [1, -1, 1]) {
        for (let step = 0; step < 30; step++) {
          const current = scroller.scrollTop;
          scroller.scrollTop = current + direction * grid.quantum + grid.writeOffset;
          expect((scroller.scrollTop - current) * direction).toBeCloseTo(grid.quantum, 3);
        }
      }
    }
  } finally {
    scroller.remove();
  }
});
