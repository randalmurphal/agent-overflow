/**
 * Clipboard helpers for mermaid diagrams.
 *
 * Three entry points, each returning the format actually written so the
 * caller can surface an accurate toast:
 *   - copyAsPNG  → rasterises the SVG onto a 2x-DPR canvas and writes
 *                  `image/png`. Falls back to SVG, then plain source.
 *   - copyAsSVG  → serialises the SVG element + writes `image/svg+xml`.
 *                  Falls back to `text/plain` (the serialised XML).
 *   - copySource → writes the original mermaid source (`text/plain`).
 *
 * Every browser we ship to supports `navigator.clipboard.write` with
 * `ClipboardItem` for `image/png` (Chromium, WebKit 13.1+, and the
 * Wails WebView2 bundle). The explicit try/catch chain keeps the call
 * safe when permissions are denied or the API is unavailable.
 *
 * WebKit constraint: `clipboard.write()` must be reached synchronously
 * within the user-gesture task — an `await` before it consumes the
 * gesture and WKWebView rejects with NotAllowedError. Async payloads
 * (PNG rasterisation) are therefore passed to `ClipboardItem` as a
 * pending Promise; the browser awaits the blob itself.
 */

export type CopyResult = 'png' | 'svg' | 'text' | 'failed';

export async function copyAsPNG(svg: SVGSVGElement): Promise<CopyResult> {
  const serialised = serialiseSvg(svg);
  if (!serialised) return 'failed';
  try {
    await writeClipboardItem(new ClipboardItem({ 'image/png': rasterisePNG(svg, serialised) }));
    return 'png';
  } catch {
    return copyAsSVGInternal(serialised);
  }
}

export async function copyAsSVG(svg: SVGSVGElement): Promise<CopyResult> {
  const serialised = serialiseSvg(svg);
  if (!serialised) return 'failed';
  return copyAsSVGInternal(serialised);
}

export async function copySource(source: string): Promise<CopyResult> {
  if (!source) return 'failed';
  try {
    await navigator.clipboard.writeText(source);
    return 'text';
  } catch {
    return 'failed';
  }
}

async function copyAsSVGInternal(serialised: string): Promise<CopyResult> {
  try {
    const svgBlob = new Blob([serialised], { type: 'image/svg+xml' });
    await writeClipboardItem(new ClipboardItem({ 'image/svg+xml': svgBlob }));
    return 'svg';
  } catch {
    try {
      await navigator.clipboard.writeText(serialised);
      return 'text';
    } catch {
      return 'failed';
    }
  }
}

// Serialise with XML namespaces normalised. Mermaid's output is
// already well-formed in practice, but external consumers of the
// pasted SVG need the `xmlns` attribute to render the file
// standalone; `XMLSerializer` preserves it when the element has it.
function serialiseSvg(svg: SVGSVGElement): string {
  let xml = new XMLSerializer().serializeToString(svg);
  if (!xml.includes('xmlns="http://www.w3.org/2000/svg"')) {
    xml = xml.replace('<svg', '<svg xmlns="http://www.w3.org/2000/svg"');
  }
  return xml;
}

// Rasterise an SVG element to a PNG Blob at 2× the device pixel ratio
// (min 2× even on standard-DPR displays) so the pasted image stays
// crisp when scaled in docs, Slack, etc. The canvas is sized from the
// SVG's viewBox when available, falling back to its rendered client
// rect — both covers mermaid's output.
async function rasterisePNG(svg: SVGSVGElement, serialised: string): Promise<Blob> {
  const { width, height } = intrinsicDimensions(svg);
  const scale = Math.max(2, Math.min(4, window.devicePixelRatio || 1));

  const url = URL.createObjectURL(new Blob([serialised], { type: 'image/svg+xml;charset=utf-8' }));
  try {
    const img = await loadImage(url);
    const canvas = document.createElement('canvas');
    canvas.width = Math.max(1, Math.round(width * scale));
    canvas.height = Math.max(1, Math.round(height * scale));
    const ctx = canvas.getContext('2d');
    if (!ctx) throw new Error('2D canvas context unavailable');
    ctx.scale(scale, scale);
    ctx.drawImage(img, 0, 0, width, height);
    const blob = await new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, 'image/png'));
    if (!blob) throw new Error('canvas.toBlob returned null');
    return blob;
  } finally {
    URL.revokeObjectURL(url);
  }
}

function intrinsicDimensions(svg: SVGSVGElement): { width: number; height: number } {
  const vb = svg.viewBox?.baseVal;
  if (vb && vb.width > 0 && vb.height > 0) {
    return { width: vb.width, height: vb.height };
  }
  const rect = svg.getBoundingClientRect();
  if (rect.width > 0 && rect.height > 0) {
    return { width: rect.width, height: rect.height };
  }
  return { width: 800, height: 600 };
}

function loadImage(url: string): Promise<HTMLImageElement> {
  return new Promise((resolve, reject) => {
    const img = new Image();
    img.onload = () => resolve(img);
    img.onerror = () => reject(new Error('SVG image decode failed'));
    img.src = url;
  });
}

// Thin wrapper so the three call sites share one "does this browser
// even have ClipboardItem.write" branch. If the API is missing we
// throw, and the fallback chain in the caller takes over.
async function writeClipboardItem(item: ClipboardItem): Promise<void> {
  if (!navigator.clipboard || typeof navigator.clipboard.write !== 'function') {
    throw new Error('navigator.clipboard.write unavailable');
  }
  await navigator.clipboard.write([item]);
}
