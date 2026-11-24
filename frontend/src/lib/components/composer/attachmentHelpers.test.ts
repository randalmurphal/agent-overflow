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
    it.each([
      ['foo.png', true],
      ['foo.JPG', true],
      ['foo.jpeg', true],
      ['foo.gif', true],
      ['foo.webp', true],
      ['foo.pdf', false],
      ['foo.txt', false],
      ['foo', false],
    ])('%s is an image extension: %s', (name, expected) => {
      expect(matchesImageExtension(name)).toBe(expected);
    });
  });

  // The kind decides the ceiling and the delivery, so it is decided the way
  // the backend decides it (`attachment.classifyUpload`) or the two disagree
  // about what the same drop is. An allowed MIME makes an image, so does an
  // image extension when the MIME is missing, and anything else is a file,
  // including image formats no provider ingests.
  describe('classifyAttachment', () => {
    it.each([
      ['image/png', 'shot', 'image'],
      ['IMAGE/JPEG', 'shot', 'image'],
      ['', 'fallback.PNG', 'image'],
      ['application/octet-stream', 'fallback.jpeg', 'image'],
      ['application/pdf', 'report.pdf', 'file'],
      ['', 'notes.txt', 'file'],
      ['image/heic', 'photo.heic', 'file'],
      ['image/bmp', 'scan.bmp', 'file'],
      ['image/svg+xml', 'logo.svg', 'file'],
    ])('MIME %j with name %s is a %s', (mime, name, kind) => {
      expect(classifyAttachment(mime, name)).toBe(kind);
    });
  });

  describe('rejectionReason', () => {
    const MB = 1024 * 1024;

    function sized(name: string, type: string, size: number): File {
      const file = new File(['x'], name, { type });
      Object.defineProperty(file, 'size', { value: size });
      return file;
    }

    // A valid image, an image-extension fallback when the MIME is missing, and
    // a type it does not recognise (a file, not a rejection) all pass.
    it.each([
      ['ok.png', 'image/png'],
      ['bad.exe', 'application/x-msdownload'],
      ['notes.txt', 'text/plain'],
      ['fallback.png', ''],
    ])('accepts %s with MIME %j', (name, type) => {
      expect(rejectionReason(new File(['x'], name, { type }), 10 * MB)).toBeNull();
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
    it.each([
      { case: 'true for drag events carrying files', event: { dataTransfer: { types: ['Files'] } }, expected: true },
      { case: 'true for image mime types in the drag payload', event: { dataTransfer: { types: ['image/png'] } }, expected: true },
      { case: 'false for non-file drags (e.g. text)', event: { dataTransfer: { types: ['text/plain'] } }, expected: false },
      { case: 'false when dataTransfer is missing', event: {}, expected: false },
    ])('is $case', ({ event, expected }) => {
      expect(hasFilePayload(event as unknown as DragEvent)).toBe(expected);
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
