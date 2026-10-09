import { describe, expect, it, vi } from 'vitest';
import { PAN_STEP_PX, PanZoom, ZOOM_STEP } from './panZoom.svelte';

/** A canvas at (100, 50) sized 800x600 whose pointer capture is recorded. */
function canvas(): HTMLDivElement & { captured: Set<number> } {
  const el = document.createElement('div') as HTMLDivElement & { captured: Set<number> };
  el.captured = new Set();
  el.getBoundingClientRect = () => new DOMRect(100, 50, 800, 600);
  el.setPointerCapture = (id: number) => { el.captured.add(id); };
  el.releasePointerCapture = (id: number) => { el.captured.delete(id); };
  el.hasPointerCapture = (id: number) => el.captured.has(id);
  return el;
}

function viewer(el = canvas(), overrides: Partial<ConstructorParameters<typeof PanZoom>[0]> = {}) {
  return new PanZoom({ canvas: () => el, minScale: 0.1, maxScale: 20, fitMaxScale: 10, ...overrides });
}

function pointer(id: number, x: number, y: number, type = 'touch'): PointerEvent {
  return {
    pointerId: id,
    pointerType: type,
    button: 0,
    clientX: x,
    clientY: y,
    preventDefault() {},
  } as unknown as PointerEvent;
}

/** Where content point (px, py) lands on screen under the current view. */
function screenPoint(view: PanZoom, el: HTMLElement, px: number, py: number): { x: number; y: number } {
  const rect = el.getBoundingClientRect();
  return { x: rect.left + view.tx + px * view.scale, y: rect.top + view.ty + py * view.scale };
}

describe('PanZoom fit', () => {
  it('centers content at the largest scale that shows all of it', () => {
    const view = viewer();
    view.fit({ width: 1600, height: 600 });
    expect(view.scale).toBe(0.5);
    expect(view.tx).toBe(0);
    expect(view.ty).toBe(150);
    expect(view.userZoomed).toBe(false);
  });

  it('enlarges small content only up to fitMaxScale', () => {
    const view = viewer(canvas(), { fitMaxScale: 1 });
    view.fit({ width: 200, height: 100 });
    expect(view.scale).toBe(1);
    expect(view.tx).toBe(300);
    expect(view.ty).toBe(250);
    const diagram = viewer();
    diagram.fit({ width: 20, height: 10 });
    expect(diagram.scale).toBe(10);
  });

  it('clears a manual zoom', () => {
    const view = viewer();
    view.fit({ width: 800, height: 600 });
    view.zoomCenter(2);
    expect(view.userZoomed).toBe(true);
    view.fit({ width: 800, height: 600 });
    expect(view.userZoomed).toBe(false);
    expect(view.scale).toBe(1);
  });

  it('does nothing without a canvas or with empty content', () => {
    const view = viewer(canvas(), { canvas: () => undefined });
    view.fit({ width: 10, height: 10 });
    expect(view.scale).toBe(1);
    const sized = viewer();
    sized.fit({ width: 0, height: 10 });
    expect(sized.scale).toBe(1);
    expect(Number.isNaN(sized.fitScale({ width: 0, height: 10 }))).toBe(true);
    // A canvas that has not been laid out yet must not fit to a zero scale.
    const flat = canvas();
    flat.getBoundingClientRect = () => new DOMRect(0, 0, 0, 0);
    const unlaid = viewer(flat);
    unlaid.fit({ width: 10, height: 10 });
    expect(unlaid.scale).toBe(1);
  });
});

describe('PanZoom zoom', () => {
  it('keeps the content under the pointer still', () => {
    const el = canvas();
    const view = viewer(el);
    view.fit({ width: 1600, height: 600 });
    const before = screenPoint(view, el, 400, 300);
    view.zoomAt(before.x, before.y, 3);
    expect(view.scale).toBe(1.5);
    const after = screenPoint(view, el, 400, 300);
    expect(after.x).toBeCloseTo(before.x, 6);
    expect(after.y).toBeCloseTo(before.y, 6);
    expect(view.userZoomed).toBe(true);
  });

  it('clamps to the scale bounds', () => {
    const view = viewer();
    view.zoomCenter(1000);
    expect(view.scale).toBe(20);
    view.zoomCenter(1 / 1000);
    expect(view.scale).toBe(0.1);
  });

  it('zooms by the wheel distance exponentially and claims the event', () => {
    const view = viewer();
    const event = { clientX: 500, clientY: 350, deltaY: -500, preventDefault: vi.fn() } as unknown as WheelEvent;
    view.onWheel(event);
    expect(event.preventDefault).toHaveBeenCalledOnce();
    expect(view.scale).toBeCloseTo(Math.exp(1), 6);
    view.onWheel({ ...event, deltaY: 500 } as unknown as WheelEvent);
    expect(view.scale).toBeCloseTo(1, 6);
  });

  it('toggles between fit and 1:1 around the point, and to 2x when they coincide', () => {
    const el = canvas();
    const view = viewer(el, { fitMaxScale: 1 });
    const big = { width: 1600, height: 600 };
    view.fit(big);
    const under = screenPoint(view, el, 1000, 200);
    view.toggle(under.x, under.y, big);
    expect(view.scale).toBe(1);
    const after = screenPoint(view, el, 1000, 200);
    expect(after.x).toBeCloseTo(under.x, 6);
    expect(after.y).toBeCloseTo(under.y, 6);
    view.toggle(under.x, under.y, big);
    expect(view.scale).toBe(0.5);
    expect(view.userZoomed).toBe(false);

    const small = { width: 400, height: 300 };
    view.fit(small);
    expect(view.scale).toBe(1);
    view.toggle(500, 350, small);
    expect(view.scale).toBe(2);
    view.toggle(500, 350, small);
    expect(view.scale).toBe(1);
  });
});

describe('PanZoom pointers', () => {
  it('drags with one pointer and captures it', () => {
    const el = canvas();
    const view = viewer(el);
    view.onPointerDown(pointer(1, 300, 300, 'mouse'));
    expect(view.panning).toBe(true);
    expect(el.captured.has(1)).toBe(true);
    view.onPointerMove(pointer(1, 340, 280));
    expect(view.tx).toBe(40);
    expect(view.ty).toBe(-20);
    expect(view.userZoomed).toBe(true);
    view.onPointerUp(pointer(1, 340, 280));
    expect(view.panning).toBe(false);
    expect(el.captured.size).toBe(0);
  });

  it('ignores a secondary mouse button and a move from an untracked pointer', () => {
    const view = viewer();
    view.onPointerDown({ ...pointer(1, 300, 300, 'mouse'), button: 2 } as unknown as PointerEvent);
    expect(view.panning).toBe(false);
    view.onPointerMove(pointer(7, 500, 500));
    expect(view.tx).toBe(0);
  });

  it('pinches around the midpoint and follows it', () => {
    const el = canvas();
    const view = viewer(el);
    view.onPointerDown(pointer(1, 400, 300));
    view.onPointerDown(pointer(2, 600, 300));
    // Spread from 200px to 400px apart: 2x around the midpoint (500, 300).
    const midContent = { x: (500 - 100 - view.tx) / view.scale, y: (300 - 50 - view.ty) / view.scale };
    view.onPointerMove(pointer(1, 300, 300));
    view.onPointerMove(pointer(2, 700, 300));
    expect(view.scale).toBeCloseTo(2, 6);
    const landed = screenPoint(view, el, midContent.x, midContent.y);
    expect(landed.x).toBeCloseTo(500, 6);
    expect(landed.y).toBeCloseTo(300, 6);
    // Both fingers slide down 50px: the view follows without changing scale.
    view.onPointerMove(pointer(1, 300, 350));
    view.onPointerMove(pointer(2, 700, 350));
    expect(view.scale).toBeCloseTo(2, 6);
    expect(screenPoint(view, el, midContent.x, midContent.y).y).toBeCloseTo(350, 6);
  });

  it('keeps dragging with the finger left after a pinch, without a jump', () => {
    const el = canvas();
    const view = viewer(el);
    view.onPointerDown(pointer(1, 400, 300));
    view.onPointerDown(pointer(2, 600, 300));
    view.onPointerMove(pointer(2, 800, 300));
    const { tx, ty, scale } = view;
    view.onPointerUp(pointer(2, 800, 300));
    expect(view.panning).toBe(true);
    expect([view.tx, view.ty, view.scale]).toEqual([tx, ty, scale]);
    view.onPointerMove(pointer(1, 410, 330));
    expect(view.tx).toBe(tx + 10);
    expect(view.ty).toBe(ty + 30);
    expect(view.scale).toBe(scale);
    view.onPointerUp(pointer(1, 410, 330));
    expect(view.panning).toBe(false);
  });
});

describe('PanZoom keys', () => {
  function key(k: string): KeyboardEvent & { preventDefault: ReturnType<typeof vi.fn> } {
    return { key: k, preventDefault: vi.fn() } as unknown as KeyboardEvent & { preventDefault: ReturnType<typeof vi.fn> };
  }

  it('zooms, fits and pans on its keys and claims them', () => {
    const view = viewer();
    const content = { width: 800, height: 600 };
    view.fit(content);
    const plus = key('+');
    expect(view.onKeydown(plus, content)).toBe(true);
    expect(plus.preventDefault).toHaveBeenCalledOnce();
    expect(view.scale).toBeCloseTo(ZOOM_STEP, 6);
    view.onKeydown(key('-'), content);
    expect(view.scale).toBeCloseTo(1, 6);
    view.onKeydown(key('ArrowLeft'), content);
    expect(view.tx).toBeCloseTo(PAN_STEP_PX, 6);
    view.onKeydown(key('ArrowDown'), content);
    expect(view.ty).toBeCloseTo(-PAN_STEP_PX, 6);
    expect(view.userZoomed).toBe(true);
    view.onKeydown(key('0'), content);
    expect([view.tx, view.ty, view.scale]).toEqual([0, 0, 1]);
    expect(view.userZoomed).toBe(false);
  });

  it('leaves other keys to the caller', () => {
    const view = viewer();
    const escape = key('Escape');
    expect(view.onKeydown(escape, { width: 1, height: 1 })).toBe(false);
    expect(escape.preventDefault).not.toHaveBeenCalled();
  });
});
