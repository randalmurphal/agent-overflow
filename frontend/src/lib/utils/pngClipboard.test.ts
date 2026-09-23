// Contract of the shared PNG clipboard write:
//   - a PNG goes on the clipboard unchanged; any other image is decoded and
//     re-encoded as PNG first,
//   - clipboard.write is reached synchronously, with the payload pending,
//   - every failure throws a toast-ready message naming the real cause.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { MAX_PNG_EDGE, asPng, requireClipboard, writePngToClipboard } from './pngClipboard';

type WriteSpy = ReturnType<typeof vi.fn<(items: ClipboardItem[]) => Promise<void>>>;

/** Clipboard whose write awaits the payload the way the engines do. */
function installClipboard(options: { writeError?: string } = {}): {
  write: WriteSpy;
  written: Blob[];
} {
  const written: Blob[] = [];
  const write = vi.fn<(items: ClipboardItem[]) => Promise<void>>(async (items) => {
    // The engine reports its OWN error for a rejected payload, never the
    // page's, which is why the module records the payload failure itself.
    try {
      for (const item of items) written.push(await item.getType('image/png'));
    } catch {
      throw new Error('NotAllowedError: clipboard write failed');
    }
    if (options.writeError) throw new Error(options.writeError);
  });
  setClipboard({ write, writeText: vi.fn() });
  return { write, written };
}

function setClipboard(value: unknown): void {
  Object.defineProperty(navigator, 'clipboard', { value, configurable: true, writable: true });
}

/** Decode and encode stubs; happy-dom has neither ImageBitmap nor a canvas. */
function stubCodec(options: { decodeFails?: boolean; encoded?: Blob | null } = {}) {
  const encoded =
    options.encoded === undefined ? new Blob(['png-bytes'], { type: 'image/png' }) : options.encoded;
  const close = vi.fn();
  const drawImage = vi.fn();
  vi.stubGlobal(
    'createImageBitmap',
    vi.fn(async () => {
      if (options.decodeFails) throw new DOMException('The source image could not be decoded.');
      return { width: 30, height: 20, close } as unknown as ImageBitmap;
    }),
  );
  const toBlob = vi
    .spyOn(HTMLCanvasElement.prototype, 'toBlob')
    .mockImplementation(function (this: HTMLCanvasElement, cb: BlobCallback) {
      cb(encoded);
    });
  vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockImplementation(
    () => ({ drawImage }) as unknown as CanvasRenderingContext2D,
  );
  return { close, drawImage, toBlob, encoded };
}

describe('pngClipboard', () => {
  const originalClipboard = navigator.clipboard;

  beforeEach(() => {
    vi.restoreAllMocks();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    setClipboard(originalClipboard);
  });

  describe('asPng', () => {
    it('hands a PNG back untouched, without decoding it', async () => {
      const codec = stubCodec();
      const png = new Blob(['original'], { type: 'image/png' });
      await expect(asPng(png)).resolves.toBe(png);
      expect(createImageBitmap).not.toHaveBeenCalled();
      expect(codec.toBlob).not.toHaveBeenCalled();
    });

    it.each(['image/jpeg', 'image/webp', 'image/gif'])(
      're-encodes %s as PNG at its natural size',
      async (type) => {
        const codec = stubCodec();
        const source = new Blob(['bytes'], { type });
        const out = await asPng(source);
        expect(out).toBe(codec.encoded);
        expect(createImageBitmap).toHaveBeenCalledWith(source);
        expect(codec.drawImage).toHaveBeenCalledWith(expect.anything(), 0, 0);
        expect(codec.toBlob.mock.calls[0][1]).toBe('image/png');
        // The decoded frame is released once drawn.
        expect(codec.close).toHaveBeenCalledTimes(1);
      },
    );

    // Chromium's createImageBitmap does not decode an SVG blob, so SVG goes
    // through an <img> and is drawn at its natural size, scaled for a crisp
    // paste and clamped to the canvas limit.
    describe('an SVG', () => {
      const OriginalImage = window.Image;
      let loaded: string[];
      let natural: { width: number; height: number };
      let fails: boolean;

      beforeEach(() => {
        loaded = [];
        natural = { width: 120, height: 40 };
        fails = false;
        class MockImage {
          onload: (() => void) | null = null;
          onerror: (() => void) | null = null;
          get naturalWidth() { return natural.width; }
          get naturalHeight() { return natural.height; }
          set src(value: string) {
            loaded.push(value);
            queueMicrotask(() => (fails ? this.onerror?.() : this.onload?.()));
          }
        }
        (window as unknown as { Image: unknown }).Image = MockImage;
      });

      afterEach(() => {
        (window as unknown as { Image: unknown }).Image = OriginalImage;
      });

      function stubCanvas() {
        const encoded = new Blob(['png-bytes'], { type: 'image/png' });
        const sizes: Array<[number, number]> = [];
        const drawImage = vi.fn();
        vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockImplementation(
          () => ({ scale: vi.fn(), drawImage }) as unknown as CanvasRenderingContext2D,
        );
        vi.spyOn(HTMLCanvasElement.prototype, 'toBlob').mockImplementation(function (
          this: HTMLCanvasElement,
          cb: BlobCallback,
        ) {
          sizes.push([this.width, this.height]);
          cb(encoded);
        });
        return { encoded, sizes, drawImage };
      }

      it('is decoded through an image element, not createImageBitmap', async () => {
        vi.stubGlobal('createImageBitmap', vi.fn(async () => {
          throw new DOMException('The source image could not be decoded.');
        }));
        const canvas = stubCanvas();
        const svg = new Blob(['<svg xmlns="http://www.w3.org/2000/svg" width="120" height="40"/>'], {
          type: 'image/svg+xml',
        });
        await expect(asPng(svg)).resolves.toBe(canvas.encoded);
        expect(createImageBitmap).not.toHaveBeenCalled();
        expect(loaded).toHaveLength(1);
        expect(loaded[0].startsWith('data:image/svg+xml')).toBe(true);
        // At least twice the natural size, so a small badge pastes crisply.
        const [[width, height]] = canvas.sizes;
        expect(width / 120).toBeGreaterThanOrEqual(2);
        expect(width / height).toBeCloseTo(3);
        expect(canvas.drawImage).toHaveBeenCalledWith(expect.anything(), 0, 0, 120, 40);
      });

      it('is clamped to the canvas edge limit', async () => {
        natural = { width: 20000, height: 5000 };
        const canvas = stubCanvas();
        await asPng(new Blob(['<svg/>'], { type: 'image/svg+xml' }));
        const [[width, height]] = canvas.sizes;
        expect(width).toBeLessThanOrEqual(MAX_PNG_EDGE);
        expect(height).toBeLessThanOrEqual(MAX_PNG_EDGE);
      });

      it('names an SVG that will not decode', async () => {
        fails = true;
        stubCanvas();
        await expect(asPng(new Blob(['nope'], { type: 'image/svg+xml' }))).rejects.toThrow(
          'the image could not be decoded',
        );
      });
    });

    it('names a decode failure', async () => {
      stubCodec({ decodeFails: true });
      await expect(asPng(new Blob(['x'], { type: 'image/jpeg' }))).rejects.toThrow(
        'the image could not be decoded',
      );
    });

    it('names a null encode and still releases the frame', async () => {
      const codec = stubCodec({ encoded: null });
      await expect(asPng(new Blob(['x'], { type: 'image/webp' }))).rejects.toThrow(
        'the image could not be encoded as a PNG',
      );
      expect(codec.close).toHaveBeenCalledTimes(1);
    });
  });

  describe('writePngToClipboard', () => {
    it('writes the produced PNG as one image/png item', async () => {
      const { write, written } = installClipboard();
      const png = new Blob(['png'], { type: 'image/png' });
      await expect(writePngToClipboard(async () => png, 'Could not copy')).resolves.toBeUndefined();
      expect(write).toHaveBeenCalledTimes(1);
      expect(write.mock.calls[0][0][0].types).toEqual(['image/png']);
      expect(written).toEqual([png]);
    });

    it('reaches clipboard.write synchronously, with the payload still pending', async () => {
      // WebKit rejects a write that resumes after an await consumed the
      // gesture, and Chromium's transient activation can expire during a
      // slow fetch. The write must be issued in the click's own task.
      const { write, written } = installClipboard();
      let deliver: (blob: Blob) => void = () => {};
      const pending = writePngToClipboard(
        () => new Promise<Blob>((resolve) => (deliver = resolve)),
        'Could not copy',
      );
      expect(write).toHaveBeenCalledTimes(1);
      const png = new Blob(['late'], { type: 'image/png' });
      deliver(png);
      await expect(pending).resolves.toBeUndefined();
      expect(written).toEqual([png]);
    });

    it('reports the payload failure rather than the clipboard DOMException', async () => {
      installClipboard();
      await expect(
        writePngToClipboard(async () => {
          throw new Error('Could not load image: this transfer is no longer available. Try again.');
        }, 'Could not copy the image'),
      ).rejects.toThrow(
        'Could not copy the image: Could not load image: this transfer is no longer available. Try again.',
      );
    });

    it('reports a rejected clipboard write', async () => {
      installClipboard({ writeError: 'Document is not focused.' });
      await expect(
        writePngToClipboard(async () => new Blob(['p'], { type: 'image/png' }), 'Could not copy the image'),
      ).rejects.toThrow('Could not copy the image: Document is not focused.');
    });

    it('reports a missing clipboard without starting the payload', async () => {
      setClipboard(undefined);
      const produce = vi.fn(async () => new Blob());
      await expect(writePngToClipboard(produce, 'Could not copy the image')).rejects.toThrow(
        'Could not copy the image: this browser provides no clipboard access',
      );
      expect(produce).not.toHaveBeenCalled();
    });
  });

  describe('requireClipboard', () => {
    it('explains an insecure context', () => {
      setClipboard(undefined);
      vi.stubGlobal('isSecureContext', false);
      expect(() => requireClipboard('write')).toThrow(
        'clipboard access needs a secure (https) connection',
      );
    });

    it('refuses a write where ClipboardItem is missing', () => {
      installClipboard();
      vi.stubGlobal('ClipboardItem', undefined);
      expect(() => requireClipboard('write')).toThrow('this browser provides no clipboard access');
      expect(requireClipboard('writeText')).toBe(navigator.clipboard);
    });
  });
});
