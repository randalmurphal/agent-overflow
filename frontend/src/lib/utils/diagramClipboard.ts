/**
 * Clipboard helpers for mermaid diagrams.
 *
 * Three entry points. Each resolves only once the clipboard actually
 * holds the content, and otherwise throws an `Error` whose message is
 * fit for a toast. There is deliberately no fallback chain: a fallback
 * puts something the reader did not ask for on the clipboard and then
 * reports success for it, which is how "copy as PNG" came to leave SVG
 * XML on the clipboard with nothing on screen to say so.
 *
 * Three engine constraints shape the implementation.
 *
 * 1. `image/svg+xml` is not a writable clipboard flavour on any engine
 *    we ship to. Chromium gates it behind the experimental `ClipboardSvg`
 *    runtime feature — off in Chrome/Edge stable, therefore off in
 *    WebView2 — and neither WebKit nor Gecko implement it at all; the
 *    write rejects with a DOMException. SVG goes on the clipboard as
 *    text, which is also the flavour anything consuming a pasted SVG
 *    (editor, `.svg` file, Figma) reads.
 *
 * 2. `navigator.clipboard.write()` must be REACHED synchronously inside
 *    the click's task. WebKit rejects a write that resumes after an
 *    `await` (the user gesture is gone by then) and Chromium's transient
 *    activation can expire across a slow rasterisation. So the PNG blob
 *    is handed to `ClipboardItem` as a still-pending Promise — supported
 *    by Chromium (≥66), required by WebKit, supported by Gecko (≥127) —
 *    and the browser awaits the raster instead of us. Nothing may be
 *    awaited before that call; `copyAsPNG` keeps its whole prologue
 *    synchronous for that reason.
 *
 * 3. The element handed in is the LIVE diagram: mermaid's own `<svg>`
 *    nested inside svelte-streamdown's outer `svg[data-mermaid-svg]`
 *    host, emitted with `width="100%"` and an inline `max-width`
 *    (`useMaxWidth`). Serialising that verbatim exports a root with no
 *    intrinsic size, which rasterises blank or cropped.
 *    `exportableDiagram` descends to the diagram root and restores real
 *    dimensions — the same normalisation `DiagramModal.normalizeSvg`
 *    performs for on-screen display. (The host `<svg>` used to carry an
 *    inline panzoom transform too; that chrome is gone, but the
 *    normalisation is still what makes the export correct.)
 */

import { errString } from './errors';

const SVG_NS = 'http://www.w3.org/2000/svg';

// Engines cap canvas dimensions (Chromium ~16384px per edge, less under
// memory pressure); past the cap `toBlob` hands back null. Clamping the
// raster scale keeps a large flowchart a slightly softer PNG instead of
// a failed copy.
const MAX_PNG_EDGE = 8192;

type ExportableDiagram = { markup: string; width: number; height: number };

export async function copyAsPNG(svg: SVGSVGElement): Promise<void> {
  // Everything up to the `write` call is synchronous — see note 2.
  let rasterFailure: unknown;
  try {
    const clipboard = requireClipboard('write');
    const png = rasterise(exportableDiagram(svg));
    // Records the real cause so the thrown message names it rather than
    // the generic DOMException the clipboard reports for a payload that
    // rejected. Registered before `ClipboardItem` observes the same
    // promise, so it also cannot surface as an unhandled rejection.
    png.catch((err: unknown) => {
      rasterFailure = err;
    });
    await clipboard.write([new ClipboardItem({ 'image/png': png })]);
  } catch (err) {
    throw new Error(`Could not copy the diagram as PNG: ${errString(rasterFailure ?? err)}`);
  }
}

export async function copyAsSVG(svg: SVGSVGElement): Promise<void> {
  await writeText(() => exportableDiagram(svg).markup, 'Could not copy the diagram as SVG');
}

export async function copySource(source: string): Promise<void> {
  await writeText(() => {
    if (!source) throw new Error('this diagram carries no source text');
    return source;
  }, 'Could not copy the diagram source');
}

/**
 * `clipboard.writeText` with the failure re-thrown under a caller-supplied
 * label. `text` is a thunk so its work also runs inside the gesture task
 * and its throws are reported the same way.
 */
async function writeText(text: () => string, failure: string): Promise<void> {
  try {
    const clipboard = requireClipboard('writeText');
    await clipboard.writeText(text());
  } catch (err) {
    throw new Error(`${failure}: ${errString(err)}`);
  }
}

/**
 * The clipboard, or a throw naming why there isn't one. `navigator.clipboard`
 * is absent outside a secure context, which is reachable today: a remote
 * client served over plain HTTP on the LAN has no clipboard API at all.
 */
function requireClipboard(need: 'write' | 'writeText'): Clipboard {
  const clipboard = typeof navigator === 'undefined' ? undefined : navigator.clipboard;
  if (
    clipboard &&
    typeof clipboard[need] === 'function' &&
    (need !== 'write' || typeof ClipboardItem === 'function')
  ) {
    return clipboard;
  }
  throw new Error(
    typeof window !== 'undefined' && window.isSecureContext === false
      ? 'clipboard access needs a secure (https) connection'
      : 'this browser provides no clipboard access',
  );
}

/**
 * A standalone copy of the diagram: document-independent markup plus the
 * pixel size to rasterise it at. See note 3 for what is being undone.
 */
function exportableDiagram(live: SVGSVGElement): ExportableDiagram {
  // svelte-streamdown renders mermaid's own `<svg>` INSIDE its outer
  // `<svg data-mermaid-svg>` host, and the host is what the context menu
  // resolves. Descend to the diagram root before exporting.
  const root = Array.from(live.children).find(isSvgElement) ?? live;
  const { width, height } = intrinsicDimensions(root, live);

  const clone = root.cloneNode(true) as SVGSVGElement;
  // Mermaid's root `style` holds only `max-width` (its real styling is in
  // a `<style>` child), so dropping the attribute wholesale is both safe
  // and the whole fix.
  clone.removeAttribute('style');
  clone.setAttribute('width', String(width));
  clone.setAttribute('height', String(height));
  if (!clone.hasAttribute('viewBox')) {
    clone.setAttribute('viewBox', `0 0 ${width} ${height}`);
  }

  // The serialiser emits the SVG namespace declaration itself for an
  // element in that namespace; the guard covers a root that somehow is
  // not, and is a string check because `setAttribute('xmlns', …)` would
  // add a second, non-namespaced attribute of the same name.
  let markup = new XMLSerializer().serializeToString(clone);
  if (!markup.includes(`xmlns="${SVG_NS}"`)) {
    markup = markup.replace('<svg', `<svg xmlns="${SVG_NS}"`);
  }
  return { markup, width, height };
}

function isSvgElement(el: Element): el is SVGSVGElement {
  return el.namespaceURI === SVG_NS && el.localName === 'svg';
}

// Prefer the viewBox: it is in user-space units, so unlike a layout box
// it does not carry any page-side scaling.
function intrinsicDimensions(
  root: SVGSVGElement,
  live: SVGSVGElement,
): { width: number; height: number } {
  const vb = root.viewBox?.baseVal;
  if (vb && vb.width > 0 && vb.height > 0) {
    return { width: vb.width, height: vb.height };
  }
  const rect = live.getBoundingClientRect();
  if (rect.width > 0 && rect.height > 0) {
    return { width: rect.width, height: rect.height };
  }
  return { width: 800, height: 600 };
}

/**
 * Rasterise the exportable markup to a PNG Blob at 2× device pixel ratio
 * (min 2× even on standard-DPR displays) so the pasted image stays crisp
 * when scaled in docs, Slack, etc.
 */
async function rasterise({ markup, width, height }: ExportableDiagram): Promise<Blob> {
  const scale = Math.min(
    Math.max(2, Math.min(4, window.devicePixelRatio || 1)),
    MAX_PNG_EDGE / Math.max(width, height),
  );

  // A `data:` URL rather than an object URL: nothing to revoke, no race
  // between the revoke and the decode, and no question about the origin
  // a blob URL inherits inside the app's webview schemes.
  const img = await loadImage(`data:image/svg+xml;charset=utf-8,${encodeURIComponent(markup)}`);
  const canvas = document.createElement('canvas');
  canvas.width = Math.max(1, Math.round(width * scale));
  canvas.height = Math.max(1, Math.round(height * scale));
  const ctx = canvas.getContext('2d');
  if (!ctx) throw new Error('no 2D canvas context is available');
  ctx.scale(scale, scale);
  ctx.drawImage(img, 0, 0, width, height);
  const blob = await new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, 'image/png'));
  if (!blob) throw new Error('the diagram could not be encoded as a PNG');
  return blob;
}

function loadImage(url: string): Promise<HTMLImageElement> {
  return new Promise((resolve, reject) => {
    const img = new Image();
    img.onload = () => resolve(img);
    img.onerror = () => reject(new Error('the diagram SVG could not be decoded as an image'));
    img.src = url;
  });
}
