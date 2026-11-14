import { describe, expect, it } from 'vitest';
import { formatBytes, imageRowHeight, imageSides, isImagePath, isSvgPath, REVIEW_IMAGE_AREA_PX, REVIEW_IMAGE_CAPTION_PX } from './reviewImageRows';

describe('review image rows', () => {
  it('knows the image types the webview renders, by extension only', () => {
    for (const path of ['a.png', 'b/c.JPG', 'd.jpeg', 'e.gif', 'f.webp', 'g.avif', 'h.bmp', 'i.ico', 'j.svg']) expect(isImagePath(path)).toBe(true);
    for (const path of ['a.pdf', 'png', 'dir.png/file', 'x.tiff', 'noext']) expect(isImagePath(path)).toBe(false);
    expect(isSvgPath('art.SVG')).toBe(true);
    expect(isSvgPath('art.svgz')).toBe(false);
  });

  it('shows one side for an add or a delete, both for a change, the new one for an edit', () => {
    expect(imageSides({ kind: 'modified' }, false)).toEqual(['old', 'new']);
    expect(imageSides({ kind: 'added' }, false)).toEqual(['new']);
    expect(imageSides({ kind: 'deleted' }, false)).toEqual(['old']);
    expect(imageSides({ kind: 'modified' }, true)).toEqual(['new']);
    expect(imageSides({ kind: 'deleted' }, true)).toEqual([]);
  });

  it('reserves a bounded height: one area per stacked side, side by side in split', () => {
    const one = imageRowHeight(1, false);
    expect(one).toBe(24 + REVIEW_IMAGE_AREA_PX + REVIEW_IMAGE_CAPTION_PX);
    expect(imageRowHeight(2, false)).toBe(one * 2 - 24 + 12);
    expect(imageRowHeight(2, true)).toBe(one);
  });

  it('formats byte sizes for captions', () => {
    expect(formatBytes(812)).toBe('812 B');
    expect(formatBytes(4300)).toBe('4.2 KB');
    expect(formatBytes(1536 * 1024)).toBe('1.5 MB');
    expect(formatBytes(30 * 1024 * 1024)).toBe('30 MB');
  });
});
