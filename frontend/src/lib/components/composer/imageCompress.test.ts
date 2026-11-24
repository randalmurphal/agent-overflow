import { describe, expect, it } from 'vitest';
import {
  MAX_COMPRESS_SOURCE_BYTES,
  compressedImageName,
  fitWithin,
  shouldCompressImage,
} from './imageCompress';

const MAX = 10 * 1024 * 1024;

function fileOfSize(size: number, name = 'shot.png', type = 'image/png'): File {
  const file = new File(['x'], name, { type });
  Object.defineProperty(file, 'size', { value: size });
  return file;
}

describe('shouldCompressImage', () => {
  it.each([
    { case: 'skips files already within the limit', size: MAX, name: undefined, type: undefined, expected: false },
    { case: 'attempts over-limit images', size: MAX + 1, name: undefined, type: undefined, expected: true },
    { case: 'refuses sources beyond the decode-safety ceiling', size: MAX_COMPRESS_SOURCE_BYTES + 1, name: undefined, type: undefined, expected: false },
    { case: 'falls back to the extension when MIME is missing', size: MAX + 1, name: 'shot.jpeg', type: '', expected: true },
    { case: 'never attempts non-image files', size: MAX + 1, name: 'dump.bin', type: 'application/octet-stream', expected: false },
  ])('$case', ({ size, name, type, expected }) => {
    expect(shouldCompressImage(fileOfSize(size, name, type), MAX)).toBe(expected);
  });
});

describe('fitWithin', () => {
  it.each([
    { case: 'leaves images under the cap untouched', w: 1920, h: 1080, fit: { width: 1920, height: 1080 } },
    { case: 'scales a wide longest edge to the cap, preserving aspect', w: 4096, h: 2048, fit: { width: 2048, height: 1024 } },
    { case: 'scales a tall longest edge to the cap, preserving aspect', w: 1000, h: 4000, fit: { width: 512, height: 2048 } },
    { case: 'never collapses a degenerate edge below 1px', w: 100_000, h: 1, fit: { width: 2048, height: 1 } },
  ])('$case', ({ w, h, fit }) => {
    expect(fitWithin(w, h, 2048)).toEqual(fit);
  });
});

describe('compressedImageName', () => {
  it.each([
    // Swaps the extension for the encoded format.
    ['screenshot.png', 'image/webp', 'screenshot.webp'],
    ['photo.jpeg', 'image/jpeg', 'photo.jpg'],
    // Handles names without an extension.
    ['pasted-image', 'image/webp', 'pasted-image.webp'],
    // Only strips the final extension.
    ['a.b.png', 'image/webp', 'a.b.webp'],
  ])('%s encoded as %s is named %s', (name, type, expected) => {
    expect(compressedImageName(name, type)).toBe(expected);
  });
});
