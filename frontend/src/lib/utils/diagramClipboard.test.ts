// Contract of the diagram copy pipeline:
//   - format selection (PNG → ClipboardItem, SVG/source → text),
//   - activation-preserving call order (write reached synchronously),
//   - loud failure (every path throws a toast-ready message, nothing
//     silently degrades to a different format).

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { copyAsPNG, copyAsSVG, copySource } from './diagramClipboard';

type WriteSpy = ReturnType<typeof vi.fn<(items: ClipboardItem[]) => Promise<void>>>;
type TextSpy = ReturnType<typeof vi.fn<(text: string) => Promise<void>>>;

function installClipboard(
  options: { writeError?: string; textError?: string } = {},
): { write: WriteSpy; writeText: TextSpy } {
  const write = vi.fn<(items: ClipboardItem[]) => Promise<void>>(async (items) => {
    // Mirror the engines: the browser awaits the payload promise itself
    // and reports its OWN failure when that promise rejects — it never
    // surfaces the page's error. That is why the module records the
    // rasterisation failure separately.
    try {
      await Promise.all(items.map((item) => item.getType('image/png')));
    } catch {
      throw new Error('NotAllowedError: clipboard write failed');
    }
    if (options.writeError) throw new Error(options.writeError);
  });
  const writeText = vi.fn<(text: string) => Promise<void>>(async () => {
    if (options.textError) throw new Error(options.textError);
  });

  setClipboard({ write, writeText });
  return { write, writeText };
}

function setClipboard(value: unknown): void {
  Object.defineProperty(navigator, 'clipboard', {
    value,
    configurable: true,
    writable: true,
  });
}

const NS = 'http://www.w3.org/2000/svg';

/**
 * The live DOM shape the markdown tree produces: mermaid's own `<svg>`
 * (percentage width + inline max-width) nested inside the outer host
 * `<svg data-mermaid-svg>`. The host's inline transform is no longer
 * written by the library, but the export must still survive one — the
 * modal's own transform host is the same shape.
 */
function makeSvg(): SVGSVGElement {
  const host = document.createElementNS(NS, 'svg');
  host.setAttribute('data-mermaid-svg', '');
  host.setAttribute('viewBox', '0 0 200 100');
  host.setAttribute('style', 'transform: translate3d(12px, 34px, 0) scale(0.71); cursor: grab');

  const diagram = document.createElementNS(NS, 'svg');
  diagram.setAttribute('viewBox', '0 0 200 100');
  diagram.setAttribute('width', '100%');
  diagram.setAttribute('style', 'max-width: 200px;');
  const rect = document.createElementNS(NS, 'rect');
  rect.setAttribute('id', 'diagram-body');
  diagram.appendChild(rect);
  host.appendChild(diagram);

  document.body.appendChild(host);
  return host as SVGSVGElement;
}

// The PNG path goes through <img> decoding + <canvas>.toBlob, which
// happy-dom does not emulate. Stub both to return deterministic values.
// `hold` keeps toBlob's callback un-invoked until `release()` so a test
// can observe the clipboard write happening while the raster is pending.
function stubRasterPipeline(
  options: { imageFails?: boolean; blob?: Blob | null; hold?: boolean } = {},
) {
  const blob = options.blob === undefined ? new Blob(['png-bytes'], { type: 'image/png' }) : options.blob;

  const OriginalImage = window.Image;
  class MockImage {
    onload: (() => void) | null = null;
    onerror: (() => void) | null = null;
    set src(_v: string) {
      // Resolve on the next microtask so `await loadImage` sees the callback.
      queueMicrotask(() => (options.imageFails ? this.onerror?.() : this.onload?.()));
    }
  }
  (window as unknown as { Image: typeof Image }).Image = MockImage as unknown as typeof Image;

  let deliverCallback: (cb: BlobCallback) => void = () => {};
  const held = new Promise<BlobCallback>((resolve) => (deliverCallback = resolve));
  const origToBlob = HTMLCanvasElement.prototype.toBlob;
  HTMLCanvasElement.prototype.toBlob = function (cb: BlobCallback) {
    if (options.hold) deliverCallback(cb);
    else cb(blob);
  } as typeof HTMLCanvasElement.prototype.toBlob;

  const origGetContext = HTMLCanvasElement.prototype.getContext;
  HTMLCanvasElement.prototype.getContext = function () {
    return { scale: vi.fn(), drawImage: vi.fn() } as unknown as CanvasRenderingContext2D;
  } as unknown as typeof HTMLCanvasElement.prototype.getContext;

  return {
    release: async () => (await held)(blob),
    restore: () => {
      (window as unknown as { Image: typeof Image }).Image = OriginalImage;
      HTMLCanvasElement.prototype.toBlob = origToBlob;
      HTMLCanvasElement.prototype.getContext = origGetContext;
    },
  };
}

describe('diagramClipboard', () => {
  let raster: ReturnType<typeof stubRasterPipeline> | null = null;
  const originalClipboard = navigator.clipboard;

  function useRaster(options: Parameters<typeof stubRasterPipeline>[0] = {}) {
    raster?.restore();
    raster = stubRasterPipeline(options);
    return raster;
  }

  beforeEach(() => {
    raster = stubRasterPipeline();
  });

  afterEach(() => {
    raster?.restore();
    raster = null;
    document.body.innerHTML = '';
    setClipboard(originalClipboard);
  });

  describe('copyAsPNG', () => {
    it('writes an image/png ClipboardItem', async () => {
      const { write, writeText } = installClipboard();
      await expect(copyAsPNG(makeSvg())).resolves.toBeUndefined();

      expect(write).toHaveBeenCalledTimes(1);
      expect(write.mock.calls[0][0][0].types).toContain('image/png');
      expect(writeText).not.toHaveBeenCalled();
    });

    it('reaches clipboard.write synchronously, with the blob still pending', async () => {
      // WebKit rejects a write that resumes after an await consumed the
      // user gesture, and Chromium's transient activation can expire
      // across the raster. The write must therefore be issued in the
      // click's own task, with the PNG handed over as a pending Promise.
      const held = useRaster({ hold: true });
      const { write } = installClipboard();
      const pending = copyAsPNG(makeSvg());
      expect(write).toHaveBeenCalledTimes(1);

      await held.release();
      await expect(pending).resolves.toBeUndefined();
    });

    it('reports the rasterisation failure rather than the clipboard DOMException', async () => {
      useRaster({ imageFails: true });
      const { write, writeText } = installClipboard();

      await expect(copyAsPNG(makeSvg())).rejects.toThrow(
        'Could not copy the diagram as PNG: the diagram SVG could not be decoded as an image',
      );
      // No second attempt in another format: a PNG copy that failed must
      // not silently leave SVG or text on the clipboard.
      expect(write).toHaveBeenCalledTimes(1);
      expect(writeText).not.toHaveBeenCalled();
    });

    it('reports a null toBlob encode', async () => {
      useRaster({ blob: null });
      installClipboard();
      await expect(copyAsPNG(makeSvg())).rejects.toThrow(
        'Could not copy the diagram as PNG: the diagram could not be encoded as a PNG',
      );
    });

    it('reports a rejected clipboard write', async () => {
      const { writeText } = installClipboard({ writeError: 'Document is not focused.' });
      await expect(copyAsPNG(makeSvg())).rejects.toThrow(
        'Could not copy the diagram as PNG: Document is not focused.',
      );
      expect(writeText).not.toHaveBeenCalled();
    });

    it('reports a missing clipboard API without touching the raster', async () => {
      setClipboard(undefined);
      await expect(copyAsPNG(makeSvg())).rejects.toThrow(
        'Could not copy the diagram as PNG: this browser provides no clipboard access',
      );
    });
  });

  describe('copyAsSVG', () => {
    it('writes markup as text, never an image/svg+xml ClipboardItem', async () => {
      // Chromium gates image/svg+xml behind the experimental ClipboardSvg
      // feature and WebKit/Gecko do not implement it, so a ClipboardItem
      // of that type can only ever reject.
      const { write, writeText } = installClipboard();
      await expect(copyAsSVG(makeSvg())).resolves.toBeUndefined();
      expect(write).not.toHaveBeenCalled();
      expect(writeText).toHaveBeenCalledTimes(1);
      expect(writeText.mock.calls[0][0]).toContain('<svg');
    });

    it('exports the diagram root without a host transform or a percentage width', async () => {
      const { writeText } = installClipboard();
      await copyAsSVG(makeSvg());
      const markup = writeText.mock.calls[0][0];

      expect(markup).toContain('xmlns="http://www.w3.org/2000/svg"');
      expect(markup).toContain('id="diagram-body"');
      // The live element carries the reader's pan/zoom and mermaid's
      // viewport-relative sizing; neither means anything outside the page.
      expect(markup).not.toContain('translate3d');
      expect(markup).not.toContain('max-width');
      // Sized in user-space units from the viewBox, so a standalone
      // consumer (and the PNG raster) has an intrinsic size to work from.
      expect(markup).toContain('width="200"');
      expect(markup).toContain('height="100"');
    });

    it('leaves the live element untouched', async () => {
      installClipboard();
      const svg = makeSvg();
      const diagram = svg.querySelector('svg') as SVGSVGElement;
      await copyAsSVG(svg);
      expect(svg.getAttribute('style')).toContain('translate3d');
      expect(diagram.getAttribute('width')).toBe('100%');
    });

    it('reports a rejected write', async () => {
      installClipboard({ textError: 'NotAllowedError' });
      await expect(copyAsSVG(makeSvg())).rejects.toThrow(
        'Could not copy the diagram as SVG: NotAllowedError',
      );
    });

    it('reports a missing clipboard API', async () => {
      setClipboard(undefined);
      await expect(copyAsSVG(makeSvg())).rejects.toThrow(
        'Could not copy the diagram as SVG: this browser provides no clipboard access',
      );
    });
  });

  describe('copySource', () => {
    it('writes the raw text', async () => {
      const { writeText } = installClipboard();
      await expect(copySource('graph TD\nA-->B')).resolves.toBeUndefined();
      expect(writeText).toHaveBeenCalledWith('graph TD\nA-->B');
    });

    it('reports an empty source without touching the clipboard', async () => {
      const { writeText } = installClipboard();
      await expect(copySource('')).rejects.toThrow(
        'Could not copy the diagram source: this diagram carries no source text',
      );
      expect(writeText).not.toHaveBeenCalled();
    });

    it('reports a rejected write', async () => {
      installClipboard({ textError: 'NotAllowedError' });
      await expect(copySource('graph TD')).rejects.toThrow(
        'Could not copy the diagram source: NotAllowedError',
      );
    });
  });
});
