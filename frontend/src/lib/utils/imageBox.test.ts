import { describe, expect, it } from 'vitest';
import { reservedImageBox } from './imageBox';
import { paintedSize } from './mediaBlobCache';

describe('reservedImageBox', () => {
  it('sizes a pixel image from its size, an authored size first', () => {
    expect(reservedImageBox({}, { width: 640, height: 480 })).toEqual({ width: '640px', height: undefined, aspectRatio: '640 / 480' });
    expect(reservedImageBox({ height: '48px' }, { width: 640, height: 480 })).toEqual({ width: '64px', height: '48px', aspectRatio: '640 / 480' });
  });

  it('gives a fluid image its ratio as a viewBox and no length of its own', () => {
    const fluid = { width: 16, height: 9, fluid: true };
    expect(reservedImageBox({}, fluid)).toEqual({ width: undefined, height: undefined, aspectRatio: undefined, viewBox: '0 0 16 9' });
    // As the painted <img>: an authored width or height, the other side
    // left to the ratio.
    expect(reservedImageBox({ width: '120px' }, fluid)).toEqual({ width: '120px', height: undefined, aspectRatio: undefined, viewBox: '0 0 16 9' });
    expect(reservedImageBox({ height: '45px' }, fluid)).toEqual({ width: 'auto', height: '45px', aspectRatio: undefined, viewBox: '0 0 16 9' });
    expect(reservedImageBox({ wrapper: '50%', width: '100%' }, fluid)).toMatchObject({ width: '100%', viewBox: '0 0 16 9' });
  });

  it('reserves nothing without a size', () => {
    expect(reservedImageBox({}, undefined)).toBeNull();
    expect(reservedImageBox({}, { width: 0, height: 0 })).toBeNull();
  });
});

describe('paintedSize', () => {
  it('marks a fluid answer fluid, and leaves every other answer as pixels', () => {
    expect(paintedSize({ width: 16, height: 9, originalWidth: 16, originalHeight: 9, fluid: true })).toEqual({ width: 16, height: 9, fluid: true });
    expect(paintedSize({ width: 320, height: 240, originalWidth: 640, originalHeight: 480, fluid: false })).toEqual({ width: 640, height: 480 });
    expect(paintedSize({ width: 320, height: 240, originalWidth: 640, originalHeight: 480 })).toEqual({ width: 640, height: 480 });
  });
});
