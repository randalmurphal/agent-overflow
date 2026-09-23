/**
 * Image clipboard writes, shared by the diagram and attachment copy paths.
 *
 * Every entry point resolves only once the clipboard holds the content and
 * otherwise throws an `Error` whose message is fit for a toast. There is no
 * fallback chain: a fallback puts something the reader did not ask for on
 * the clipboard and then reports success for it.
 *
 * Engine constraints:
 *
 * 1. `image/png` is the only image flavour every engine we ship to can
 *    write. Chromium gates `image/svg+xml` behind an experimental flag and
 *    WebKit and Gecko do not implement it; JPEG, WebP and GIF are not
 *    writable anywhere. Any other image is re-encoded to PNG first
 *    (`asPng`).
 *
 * 2. `navigator.clipboard.write()` must be REACHED synchronously inside the
 *    click's task. WebKit rejects a write that resumes after an `await` (the
 *    user gesture is gone by then) and Chromium's transient activation can
 *    expire across a slow fetch or encode. So the PNG is handed to
 *    `ClipboardItem` as a still-pending Promise (Chromium, WebKit, Gecko
 *    127+) and the browser awaits it instead of us. `writePngToClipboard`
 *    keeps everything before that call synchronous; its `png` producer is
 *    started inside it and may await whatever it needs.
 *
 * 3. `navigator.clipboard` is absent outside a secure context, which a
 *    remote client served over plain HTTP on the LAN is (`requireClipboard`).
 */

import { reportCopyFailure } from './clipboard';
import { errString } from './errors';

/**
 * The clipboard, or a throw naming why there isn't one.
 */
export function requireClipboard(need: 'write' | 'writeText'): Clipboard {
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
 * Put the PNG `png()` produces on the clipboard. `png` runs only after the
 * clipboard is known to exist, and its failure is what the thrown message
 * names: the browser reports its own generic DOMException for a payload
 * promise that rejected. The failure is also recorded through
 * `reportCopyFailure`, because the toast is gone once dismissed.
 */
export async function writePngToClipboard(
  png: () => Promise<Blob>,
  failure: string,
): Promise<void> {
  // Everything up to the `write` call is synchronous; see note 2.
  let payloadFailure: unknown;
  try {
    const clipboard = requireClipboard('write');
    const payload = png();
    // Registered before `ClipboardItem` observes the same promise, so a
    // rejection is recorded here and cannot surface as unhandled.
    payload.catch((err: unknown) => {
      payloadFailure = err;
    });
    await clipboard.write([new ClipboardItem({ 'image/png': payload })]);
  } catch (err) {
    const cause = payloadFailure ?? err;
    reportCopyFailure(`${failure}:`, cause);
    throw new Error(`${failure}: ${errString(cause)}`);
  }
}

// Engines cap canvas dimensions (Chromium ~16384px per edge, less under
// memory pressure); past the cap `toBlob` hands back null. Clamping the
// raster scale keeps a large SVG a slightly softer PNG instead of a failed
// copy.
export const MAX_PNG_EDGE = 8192;

/**
 * The image as PNG bytes: a PNG as it is, anything else decoded and drawn
 * onto a canvas at its natural size and encoded. An animated GIF yields its
 * first frame. An SVG is rasterised by `rasteriseSvg`, because Chromium's
 * `createImageBitmap` does not decode an SVG blob.
 */
export async function asPng(image: Blob): Promise<Blob> {
  if (image.type === 'image/png') return image;
  if (/^image\/svg\+xml\b/i.test(image.type)) {
    const markup = await image.text();
    return rasteriseSvg(markup, null, {
      decode: 'the image could not be decoded',
      encode: 'the image could not be encoded as a PNG',
    });
  }
  let bitmap: ImageBitmap;
  try {
    bitmap = await createImageBitmap(image);
  } catch {
    throw new Error('the image could not be decoded');
  }
  const canvas = document.createElement('canvas');
  try {
    canvas.width = bitmap.width;
    canvas.height = bitmap.height;
    const ctx = canvas.getContext('2d');
    if (!ctx) throw new Error('no 2D canvas context is available');
    ctx.drawImage(bitmap, 0, 0);
    const png = await new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, 'image/png'));
    if (!png) throw new Error('the image could not be encoded as a PNG');
    return png;
  } finally {
    bitmap.close();
    // Release the backing store now rather than at the next collection: a
    // photo-sized canvas holds tens of megabytes.
    canvas.width = 0;
    canvas.height = 0;
  }
}

/**
 * Rasterise SVG markup to a PNG at `size`, or at the image's natural size
 * when `size` is null, scaled to at least 2x (up to 4x on a dense display)
 * so a pasted vector stays crisp, and clamped to `MAX_PNG_EDGE`. The two
 * messages name the failure for the caller's subject.
 */
export async function rasteriseSvg(
  markup: string,
  size: { width: number; height: number } | null,
  failures: { decode: string; encode: string },
): Promise<Blob> {
  // A `data:` URL rather than an object URL: nothing to revoke, no race
  // between the revoke and the decode, and no question about the origin a
  // blob URL inherits inside the app's webview schemes. An SVG loaded as an
  // image runs no script.
  const img = await loadImage(`data:image/svg+xml;charset=utf-8,${encodeURIComponent(markup)}`, failures.decode);
  // An SVG with no intrinsic size reports 0; the CSS default object size
  // is what a browser would lay it out at.
  const width = size?.width || img.naturalWidth || 300;
  const height = size?.height || img.naturalHeight || 150;
  const scale = Math.min(
    Math.max(2, Math.min(4, window.devicePixelRatio || 1)),
    MAX_PNG_EDGE / Math.max(width, height),
  );
  const canvas = document.createElement('canvas');
  try {
    canvas.width = Math.max(1, Math.round(width * scale));
    canvas.height = Math.max(1, Math.round(height * scale));
    const ctx = canvas.getContext('2d');
    if (!ctx) throw new Error('no 2D canvas context is available');
    ctx.scale(scale, scale);
    ctx.drawImage(img, 0, 0, width, height);
    const blob = await new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, 'image/png'));
    if (!blob) throw new Error(failures.encode);
    return blob;
  } finally {
    canvas.width = 0;
    canvas.height = 0;
  }
}

function loadImage(url: string, failure: string): Promise<HTMLImageElement> {
  return new Promise((resolve, reject) => {
    const img = new Image();
    img.onload = () => resolve(img);
    img.onerror = () => reject(new Error(failure));
    img.src = url;
  });
}
