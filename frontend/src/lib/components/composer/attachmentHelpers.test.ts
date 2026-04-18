import { describe, expect, it } from 'vitest';

import {
  extractClipboardImages,
  hasImagePayload,
  matchesImageExtension,
  rejectionReason,
} from './attachmentHelpers';

describe('attachmentHelpers', () => {
  describe('matchesImageExtension', () => {
    it('recognises common image extensions', () => {
      expect(matchesImageExtension('foo.png')).toBe(true);
      expect(matchesImageExtension('foo.JPG')).toBe(true);
      expect(matchesImageExtension('foo.jpeg')).toBe(true);
      expect(matchesImageExtension('foo.gif')).toBe(true);
      expect(matchesImageExtension('foo.webp')).toBe(true);
    });

    it('rejects non-image extensions', () => {
      expect(matchesImageExtension('foo.pdf')).toBe(false);
      expect(matchesImageExtension('foo.txt')).toBe(false);
      expect(matchesImageExtension('foo')).toBe(false);
    });
  });

  describe('rejectionReason', () => {
    const MB = 1024 * 1024;

    it('returns null for a valid image file', () => {
      const file = new File(['x'], 'ok.png', { type: 'image/png' });
      expect(rejectionReason(file, 10 * MB)).toBeNull();
    });

    it('rejects unsupported MIME types', () => {
      const file = new File(['x'], 'bad.exe', { type: 'application/x-msdownload' });
      expect(rejectionReason(file, 10 * MB)).toMatch(/Unsupported/);
    });

    it('accepts an image-extension fallback when MIME is missing', () => {
      const file = new File(['x'], 'fallback.png', { type: '' });
      expect(rejectionReason(file, 10 * MB)).toBeNull();
    });

    it('rejects files that exceed the size cap', () => {
      const file = new File(['x'.repeat(2 * MB)], 'big.png', { type: 'image/png' });
      expect(rejectionReason(file, 1 * MB)).toMatch(/MB; limit/);
    });
  });

  describe('hasImagePayload', () => {
    it('is true for drag events carrying files', () => {
      const event = { dataTransfer: { types: ['Files'] } } as unknown as DragEvent;
      expect(hasImagePayload(event)).toBe(true);
    });

    it('is true for image mime types in the drag payload', () => {
      const event = { dataTransfer: { types: ['image/png'] } } as unknown as DragEvent;
      expect(hasImagePayload(event)).toBe(true);
    });

    it('is false for non-file drags (e.g. text)', () => {
      const event = { dataTransfer: { types: ['text/plain'] } } as unknown as DragEvent;
      expect(hasImagePayload(event)).toBe(false);
    });

    it('is false when dataTransfer is missing', () => {
      const event = {} as unknown as DragEvent;
      expect(hasImagePayload(event)).toBe(false);
    });
  });

  describe('extractClipboardImages', () => {
    it('collects every image/file item from the clipboard', () => {
      const imgFile = new File(['x'], 'a.png', { type: 'image/png' });
      const event = {
        clipboardData: {
          items: [
            { kind: 'file', type: 'image/png', getAsFile: () => imgFile },
            { kind: 'string', type: 'text/plain', getAsFile: () => null },
            { kind: 'file', type: 'application/pdf', getAsFile: () => null },
          ],
        },
      } as unknown as ClipboardEvent;
      const files = extractClipboardImages(event);
      expect(files).toEqual([imgFile]);
    });

    it('returns an empty array when clipboardData is missing', () => {
      const event = {} as unknown as ClipboardEvent;
      expect(extractClipboardImages(event)).toEqual([]);
    });
  });
});
