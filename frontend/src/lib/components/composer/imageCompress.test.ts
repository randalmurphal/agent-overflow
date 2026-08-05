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
  it('skips files already within the limit', () => {
    expect(shouldCompressImage(fileOfSize(MAX), MAX)).toBe(false);
  });

  it('attempts over-limit images', () => {
    expect(shouldCompressImage(fileOfSize(MAX + 1), MAX)).toBe(true);
  });

  it('refuses sources beyond the decode-safety ceiling', () => {
    expect(shouldCompressImage(fileOfSize(MAX_COMPRESS_SOURCE_BYTES + 1), MAX)).toBe(false);
  });

  it('falls back to the extension when MIME is missing', () => {
    expect(shouldCompressImage(fileOfSize(MAX + 1, 'shot.jpeg', ''), MAX)).toBe(true);
  });

  it('never attempts non-image files', () => {
    expect(shouldCompressImage(fileOfSize(MAX + 1, 'dump.bin', 'application/octet-stream'), MAX)).toBe(false);
  });
});

describe('fitWithin', () => {
  it('leaves images under the cap untouched', () => {
    expect(fitWithin(1920, 1080, 2048)).toEqual({ width: 1920, height: 1080 });
  });

  it('scales the longest edge to the cap, preserving aspect', () => {
    expect(fitWithin(4096, 2048, 2048)).toEqual({ width: 2048, height: 1024 });
    expect(fitWithin(1000, 4000, 2048)).toEqual({ width: 512, height: 2048 });
  });

  it('never collapses a degenerate edge below 1px', () => {
    expect(fitWithin(100_000, 1, 2048)).toEqual({ width: 2048, height: 1 });
  });
});

describe('compressedImageName', () => {
  it('swaps the extension for the encoded format', () => {
    expect(compressedImageName('screenshot.png', 'image/webp')).toBe('screenshot.webp');
    expect(compressedImageName('photo.jpeg', 'image/jpeg')).toBe('photo.jpg');
  });

  it('handles names without an extension', () => {
    expect(compressedImageName('pasted-image', 'image/webp')).toBe('pasted-image.webp');
  });

  it('only strips the final extension', () => {
    expect(compressedImageName('a.b.png', 'image/webp')).toBe('a.b.webp');
  });
});
