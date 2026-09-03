import { describe, expect, it } from 'vitest';

import {
  classifyAttachment,
  extractClipboardImages,
  hasFilePayload,
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

  // The kind decides the ceiling and the delivery, so it is decided the way
  // the backend decides it (`attachment.classifyUpload`) or the two disagree
  // about what the same drop is.
  describe('classifyAttachment', () => {
    it('is an image for an allowed MIME', () => {
      expect(classifyAttachment('image/png', 'shot')).toBe('image');
      expect(classifyAttachment('IMAGE/JPEG', 'shot')).toBe('image');
    });

    it('is an image on the extension when the MIME is missing', () => {
      expect(classifyAttachment('', 'fallback.PNG')).toBe('image');
      expect(classifyAttachment('application/octet-stream', 'fallback.jpeg')).toBe('image');
    });

    it('is a file for anything else, including image formats no provider ingests', () => {
      expect(classifyAttachment('application/pdf', 'report.pdf')).toBe('file');
      expect(classifyAttachment('', 'notes.txt')).toBe('file');
      expect(classifyAttachment('image/heic', 'photo.heic')).toBe('file');
      expect(classifyAttachment('image/bmp', 'scan.bmp')).toBe('file');
      expect(classifyAttachment('image/svg+xml', 'logo.svg')).toBe('file');
    });
  });

  describe('rejectionReason', () => {
    const MB = 1024 * 1024;

    function sized(name: string, type: string, size: number): File {
      const file = new File(['x'], name, { type });
      Object.defineProperty(file, 'size', { value: size });
      return file;
    }

    it('returns null for a valid image file', () => {
      const file = new File(['x'], 'ok.png', { type: 'image/png' });
      expect(rejectionReason(file, 10 * MB)).toBeNull();
    });

    it('accepts a type it does not recognise — it is a file, not a rejection', () => {
      expect(rejectionReason(new File(['x'], 'bad.exe', { type: 'application/x-msdownload' }), 10 * MB))
        .toBeNull();
      expect(rejectionReason(new File(['x'], 'notes.txt', { type: 'text/plain' }), 10 * MB)).toBeNull();
    });

    it('accepts an image-extension fallback when MIME is missing', () => {
      const file = new File(['x'], 'fallback.png', { type: '' });
      expect(rejectionReason(file, 10 * MB)).toBeNull();
    });

    it('rejects an image past the caller image cap, naming the kind', () => {
      const file = new File(['x'.repeat(2 * MB)], 'big.png', { type: 'image/png' });
      expect(rejectionReason(file, 1 * MB)).toBe('big.png is 2.0 MB; limit for images is 1 MB');
    });

    it('holds a file to the 50 MiB file ceiling, not the image one', () => {
      expect(rejectionReason(sized('report.pdf', 'application/pdf', 40 * MB), 1 * MB)).toBeNull();
      expect(rejectionReason(sized('report.pdf', 'application/pdf', 60 * MB), 1 * MB))
        .toBe('report.pdf is 60.0 MB; limit for files is 50 MB');
    });
  });

  describe('hasFilePayload', () => {
    it('is true for drag events carrying files', () => {
      const event = { dataTransfer: { types: ['Files'] } } as unknown as DragEvent;
      expect(hasFilePayload(event)).toBe(true);
    });

    it('is true for image mime types in the drag payload', () => {
      const event = { dataTransfer: { types: ['image/png'] } } as unknown as DragEvent;
      expect(hasFilePayload(event)).toBe(true);
    });

    it('is false for non-file drags (e.g. text)', () => {
      const event = { dataTransfer: { types: ['text/plain'] } } as unknown as DragEvent;
      expect(hasFilePayload(event)).toBe(false);
    });

    it('is false when dataTransfer is missing', () => {
      const event = {} as unknown as DragEvent;
      expect(hasFilePayload(event)).toBe(false);
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
