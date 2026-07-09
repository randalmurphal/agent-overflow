import { afterEach, describe, expect, it, vi } from 'vitest';
import { createResizeGesture, type ResizeGestureOptions } from './resizeGesture.svelte';

function makeTarget(): HTMLElement {
  const el = document.createElement('div');
  el.setPointerCapture = () => {};
  el.releasePointerCapture = () => {};
  return el;
}

function pointerDown(target: HTMLElement, button: number): PointerEvent {
  const event = new MouseEvent('pointerdown', {
    button,
    clientX: 100,
    bubbles: true,
    cancelable: true,
  });
  Object.defineProperty(event, 'pointerId', { configurable: true, value: 1 });
  Object.defineProperty(event, 'currentTarget', { configurable: true, value: target });
  return event as unknown as PointerEvent;
}

function options(overrides: Partial<ResizeGestureOptions> = {}): ResizeGestureOptions {
  return {
    axis: 'x',
    cursor: 'col-resize',
    currentSize: 200,
    minSize: 100,
    maxSize: 400,
    direction: 1,
    onResizeLive: () => {},
    onResizeEnd: () => {},
    ...overrides,
  };
}

describe('createResizeGesture', () => {
  afterEach(() => {
    document.body.style.cursor = '';
    document.body.style.userSelect = '';
  });

  it('ignores non-primary buttons (no drag, no side effects)', () => {
    const onResizeLive = vi.fn();
    const acquireLease = vi.fn();
    const gesture = createResizeGesture(() => options({ onResizeLive, acquireLease }));

    gesture.onPointerDown(pointerDown(makeTarget(), 2));

    expect(gesture.dragging).toBe(false);
    expect(document.body.style.cursor).toBe('');
    expect(acquireLease).not.toHaveBeenCalled();
    expect(onResizeLive).not.toHaveBeenCalled();
  });

  it('starts a drag on the primary button', () => {
    const acquireLease = vi.fn(() => () => {});
    const gesture = createResizeGesture(() => options({ acquireLease }));

    gesture.onPointerDown(pointerDown(makeTarget(), 0));

    expect(gesture.dragging).toBe(true);
    expect(document.body.style.cursor).toBe('col-resize');
    expect(acquireLease).toHaveBeenCalledTimes(1);
    gesture.destroy();
  });
});
