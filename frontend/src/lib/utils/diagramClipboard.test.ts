import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { copyAsPNG, copyAsSVG, copySource } from './diagramClipboard';

type WriteSpy = ReturnType<typeof vi.fn<(items: ClipboardItem[]) => Promise<void>>>;
type TextSpy = ReturnType<typeof vi.fn<(text: string) => Promise<void>>>;

function installClipboard(options: {
  writeSucceeds?: boolean | ((items: ClipboardItem[]) => boolean);
  textSucceeds?: boolean;
} = {}): { write: WriteSpy; writeText: TextSpy } {
  const writeSucceeds = options.writeSucceeds ?? true;
  const textSucceeds = options.textSucceeds ?? true;

  const write = vi.fn<(items: ClipboardItem[]) => Promise<void>>(async (items) => {
    const ok = typeof writeSucceeds === 'function' ? writeSucceeds(items) : writeSucceeds;
    if (!ok) throw new Error('mock: write rejected');
  });
  const writeText = vi.fn<(text: string) => Promise<void>>(async () => {
    if (!textSucceeds) throw new Error('mock: writeText rejected');
  });

  Object.defineProperty(navigator, 'clipboard', {
    value: { write, writeText },
    configurable: true,
    writable: true,
  });
  return { write, writeText };
}

function makeSvg(): SVGSVGElement {
  const ns = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(ns, 'svg');
  svg.setAttribute('xmlns', ns);
  svg.setAttribute('viewBox', '0 0 100 50');
  svg.setAttribute('width', '100');
  svg.setAttribute('height', '50');
  const rect = document.createElementNS(ns, 'rect');
  rect.setAttribute('width', '100');
  rect.setAttribute('height', '50');
  svg.appendChild(rect);
  return svg as SVGSVGElement;
}

// The PNG path goes through <canvas>.toBlob + <img> decoding, which
// jsdom does not emulate. Stub both to return deterministic values.
function stubRasterPipeline() {
  const origCreateObjectURL = URL.createObjectURL;
  const origRevokeObjectURL = URL.revokeObjectURL;

  URL.createObjectURL = vi.fn(() => 'blob:mock-url');
  URL.revokeObjectURL = vi.fn();

  const OriginalImage = window.Image;
  class MockImage {
    onload: (() => void) | null = null;
    onerror: (() => void) | null = null;
    set src(_v: string) {
      // Resolve on the next microtask so `await loadImage` sees the callback.
      queueMicrotask(() => this.onload?.());
    }
  }
  (window as unknown as { Image: typeof Image }).Image = MockImage as unknown as typeof Image;

  const origToBlob = HTMLCanvasElement.prototype.toBlob;
  HTMLCanvasElement.prototype.toBlob = function (cb: BlobCallback) {
    cb(new Blob(['png-bytes'], { type: 'image/png' }));
  } as typeof HTMLCanvasElement.prototype.toBlob;

  const origGetContext = HTMLCanvasElement.prototype.getContext;
  HTMLCanvasElement.prototype.getContext = function () {
    return {
      scale: vi.fn(),
      drawImage: vi.fn(),
    } as unknown as CanvasRenderingContext2D;
  } as unknown as typeof HTMLCanvasElement.prototype.getContext;

  return () => {
    URL.createObjectURL = origCreateObjectURL;
    URL.revokeObjectURL = origRevokeObjectURL;
    (window as unknown as { Image: typeof Image }).Image = OriginalImage;
    HTMLCanvasElement.prototype.toBlob = origToBlob;
    HTMLCanvasElement.prototype.getContext = origGetContext;
  };
}

describe('diagramClipboard', () => {
  let restoreRaster: (() => void) | null = null;
  const originalClipboard = navigator.clipboard;

  beforeEach(() => {
    restoreRaster = stubRasterPipeline();
  });

  afterEach(() => {
    restoreRaster?.();
    restoreRaster = null;
    if (originalClipboard) {
      Object.defineProperty(navigator, 'clipboard', {
        value: originalClipboard,
        configurable: true,
        writable: true,
      });
    }
  });

  it('copyAsPNG writes an image/png ClipboardItem', async () => {
    const { write } = installClipboard();
    const result = await copyAsPNG(makeSvg());
    expect(result).toBe('png');
    expect(write).toHaveBeenCalledTimes(1);
    const item = write.mock.calls[0][0][0];
    expect(item.types).toContain('image/png');
  });

  it('copyAsPNG falls back to image/svg+xml when PNG write rejects', async () => {
    // Only accept the SVG MIME — pretend the host rejects PNG writes.
    const { write } = installClipboard({
      writeSucceeds: (items) => items[0].types.includes('image/svg+xml'),
    });
    const result = await copyAsPNG(makeSvg());
    expect(result).toBe('svg');
    // Two attempts: PNG (rejected) then SVG (accepted).
    expect(write).toHaveBeenCalledTimes(2);
    expect(write.mock.calls[0][0][0].types).toContain('image/png');
    expect(write.mock.calls[1][0][0].types).toContain('image/svg+xml');
  });

  it('copyAsPNG falls back all the way to text when both MIMEs reject', async () => {
    const { write, writeText } = installClipboard({
      writeSucceeds: false,
      textSucceeds: true,
    });
    const result = await copyAsPNG(makeSvg());
    expect(result).toBe('text');
    expect(write).toHaveBeenCalledTimes(2); // PNG, then SVG
    expect(writeText).toHaveBeenCalledTimes(1);
    expect(writeText.mock.calls[0][0]).toContain('<svg');
  });

  it('copyAsPNG returns failed when every path rejects', async () => {
    installClipboard({ writeSucceeds: false, textSucceeds: false });
    const result = await copyAsPNG(makeSvg());
    expect(result).toBe('failed');
  });

  it('copyAsSVG writes image/svg+xml and ensures the xmlns is present', async () => {
    const { write } = installClipboard();
    const result = await copyAsSVG(makeSvg());
    expect(result).toBe('svg');
    const item = write.mock.calls[0][0][0];
    expect(item.types).toContain('image/svg+xml');
    const blob = await item.getType('image/svg+xml');
    const text = await blob.text();
    expect(text).toContain('xmlns="http://www.w3.org/2000/svg"');
  });

  it('copySource writes the raw text', async () => {
    const { writeText } = installClipboard();
    const result = await copySource('graph TD\nA-->B');
    expect(result).toBe('text');
    expect(writeText).toHaveBeenCalledWith('graph TD\nA-->B');
  });

  it('copySource returns failed on an empty source without touching the clipboard', async () => {
    const { writeText } = installClipboard();
    const result = await copySource('');
    expect(result).toBe('failed');
    expect(writeText).not.toHaveBeenCalled();
  });
});
