import { afterEach, describe, expect, it } from 'vitest';

const mounted: HTMLElement[] = [];

afterEach(() => {
  for (const element of mounted.splice(0)) element.remove();
});

function mountScroller(): HTMLElement {
  const scroller = document.createElement('div');
  scroller.style.cssText = [
    'position: fixed',
    'left: -10000px',
    'top: 0',
    'width: 100px',
    'height: 100px',
    'overflow: auto',
  ].join(';');
  const content = document.createElement('div');
  content.style.height = '1000px';
  scroller.appendChild(content);
  document.body.appendChild(scroller);
  mounted.push(scroller);
  return scroller;
}

describe('programmatic scrollTop readback', () => {
  it('quantizes fractional writes to whole CSS pixels in real Chromium', () => {
    const scroller = mountScroller();
    const writes = [0.1, 0.25, 0.4, 0.5, 0.6, 0.75, 1.1, 1.25, 1.5, 1.75, 2.4];

    for (const requested of writes) {
      scroller.scrollTop = requested;
      expect(scroller.scrollTop, `requested ${requested}px at DPR ${devicePixelRatio}`)
        .toBe(Math.round(requested));
    }
  });
});
