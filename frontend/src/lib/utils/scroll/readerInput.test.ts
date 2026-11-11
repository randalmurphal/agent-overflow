import { afterEach, describe, expect, it } from 'vitest';
import { watchReaderScrollInput } from './readerInput';

// Which input events on a scroller are the reader scrolling it.
describe('watchReaderScrollInput', () => {
  let stop = () => {};
  afterEach(() => {
    stop();
    document.body.innerHTML = '';
  });

  function setup() {
    const host = document.createElement('div');
    const scroller = document.createElement('div');
    const row = document.createElement('div');
    const field = document.createElement('textarea');
    const outside = document.createElement('button');
    scroller.append(row, field);
    host.append(scroller, outside);
    document.body.append(host);
    let inputs = 0;
    stop = watchReaderScrollInput(host, () => scroller, () => { inputs += 1; });
    return { host, scroller, row, field, outside, inputs: () => inputs };
  }

  const touch = (target: Element, type: string, x: number, y: number) => {
    const event = new Event(type, { bubbles: true }) as Event & { touches: { clientX: number; clientY: number }[] };
    Object.defineProperty(event, 'touches', { value: [{ clientX: x, clientY: y }] });
    target.dispatchEvent(event);
  };

  it('counts a wheel, either axis, and not a pinch zoom', () => {
    const { row, inputs } = setup();
    row.dispatchEvent(new WheelEvent('wheel', { deltaY: 40, bubbles: true }));
    row.dispatchEvent(new WheelEvent('wheel', { deltaX: 40, bubbles: true }));
    const pinch = new WheelEvent('wheel', { deltaY: 40, bubbles: true });
    // happy-dom does not read ctrlKey from a WheelEvent's init.
    Object.defineProperty(pinch, 'ctrlKey', { value: true });
    row.dispatchEvent(pinch);
    expect(inputs()).toBe(2);
  });

  it('counts scroll keys, and not keys typed into an editor or other keys', () => {
    const { row, field, inputs } = setup();
    for (const key of ['PageDown', 'PageUp', 'ArrowDown', 'ArrowUp', 'Home', 'End', 'ArrowLeft', 'ArrowRight', ' ']) {
      row.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true }));
    }
    field.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    row.dispatchEvent(new KeyboardEvent('keydown', { key: 'a', bubbles: true }));
    expect(inputs()).toBe(9);
  });

  it('counts a touch drag, and not a still finger', () => {
    const { row, inputs } = setup();
    touch(row, 'touchstart', 10, 10);
    touch(row, 'touchmove', 10, 11);
    expect(inputs()).toBe(0);
    touch(row, 'touchmove', 10, 40);
    expect(inputs()).toBe(1);
  });

  it('counts a middle-button press, and not a primary press on the content', () => {
    const { row, inputs } = setup();
    row.dispatchEvent(new PointerEvent('pointerdown', { button: 0, bubbles: true, isPrimary: true }));
    row.dispatchEvent(new PointerEvent('pointerdown', { button: 1, bubbles: true }));
    expect(inputs()).toBe(1);
  });

  it('ignores input outside the scroller, and nothing after it stops', () => {
    const { outside, row, inputs } = setup();
    outside.dispatchEvent(new WheelEvent('wheel', { deltaY: 40, bubbles: true }));
    outside.dispatchEvent(new KeyboardEvent('keydown', { key: 'PageDown', bubbles: true }));
    stop();
    row.dispatchEvent(new WheelEvent('wheel', { deltaY: 40, bubbles: true }));
    expect(inputs()).toBe(0);
  });
});
