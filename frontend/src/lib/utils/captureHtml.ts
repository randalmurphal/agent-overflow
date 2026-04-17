// Renders an HTML artifact to a PNG image in the browser.
//
// We don't reach into the preview iframe directly — its sandbox attribute
// can block cross-origin access to the document, and we don't want the
// preview UI to have to relax sandboxing just so screenshots work. Instead
// we mount a temporary hidden iframe, load the HTML via srcdoc (same
// origin), wait for it to settle, capture it with modern-screenshot, and
// tear it down. The caller gets a Blob they can upload as an attachment.
//
// Rendering fidelity matches what the user saw in the preview (same HTML,
// same layout engine, same browser) with two caveats:
//   1. Fonts: only what the iframe can load (system fonts + any @font-face
//      the HTML declares) are rendered. Missing fonts fall back.
//   2. Cross-origin resources (remote images, fetched CSS) render if the
//      iframe's normal network access works. Happy-dom in tests doesn't
//      ship real DOM rendering, so tests mock this module.

import { domToPng } from 'modern-screenshot';

export interface CaptureOptions {
  /** Target viewport width in CSS pixels. Defaults to 1280. */
  width?: number;
  /**
   * How long to wait after `load` fires before capturing. Lets CSS
   * animations / @font-face settle. Defaults to 150ms.
   */
  settleMs?: number;
}

/**
 * Render an HTML document string to a PNG Blob. Resolves when the capture
 * completes; rejects on DOM errors, load timeouts, or capture failures.
 * Always cleans up the temporary iframe before returning.
 */
export async function captureHtmlToPng(html: string, options: CaptureOptions = {}): Promise<Blob> {
  const width = options.width ?? 1280;
  const settleMs = options.settleMs ?? 150;

  const iframe = document.createElement('iframe');
  iframe.setAttribute('aria-hidden', 'true');
  iframe.style.position = 'fixed';
  iframe.style.left = '-10000px';
  iframe.style.top = '0';
  iframe.style.width = `${width}px`;
  iframe.style.border = '0';
  iframe.style.visibility = 'hidden';
  // Intentionally no sandbox — we trust our own rendered HTML enough to
  // reach into its document for the capture. The iframe is destroyed
  // right after.
  iframe.srcdoc = html;

  document.body.appendChild(iframe);

  try {
    await waitForIframeLoad(iframe);
    // Give late resources a moment to settle.
    await new Promise<void>((resolve) => setTimeout(resolve, settleMs));

    const body = iframe.contentDocument?.body;
    if (!body) {
      throw new Error('Screenshot target is missing — iframe document did not populate.');
    }
    // Use the document element (not body) so the full page chrome is
    // captured, including <html> background styles.
    const target = iframe.contentDocument?.documentElement ?? body;

    // Auto-size the iframe to the target's scroll height so tall pages
    // render in full rather than being clipped to viewport height.
    const scrollHeight = target.scrollHeight || body.scrollHeight || 800;
    iframe.style.height = `${scrollHeight}px`;

    const dataUrl = await domToPng(target, {
      width,
      height: scrollHeight,
      backgroundColor: '#ffffff',
      // Cross-origin images / stylesheets are rendered as-is when CORS
      // allows. Everything else degrades silently rather than aborting
      // the capture — a partial screenshot is more useful to the user
      // than an error.
      fetch: { bypassingCache: true },
    });

    return await dataUrlToBlob(dataUrl);
  } finally {
    iframe.remove();
  }
}

function waitForIframeLoad(iframe: HTMLIFrameElement, timeoutMs = 10_000): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    let done = false;
    const finish = (err?: Error) => {
      if (done) return;
      done = true;
      iframe.removeEventListener('load', onLoad);
      if (err) reject(err);
      else resolve();
    };
    const onLoad = () => finish();
    iframe.addEventListener('load', onLoad, { once: true });
    setTimeout(() => finish(new Error(`Iframe load timed out after ${timeoutMs}ms`)), timeoutMs);
  });
}

async function dataUrlToBlob(dataUrl: string): Promise<Blob> {
  const response = await fetch(dataUrl);
  return response.blob();
}

/**
 * Convert a PNG Blob to a base64 string suitable for UploadAttachment,
 * which expects the raw (un-prefixed) base64 payload.
 */
export async function blobToBase64(blob: Blob): Promise<string> {
  return new Promise<string>((resolve, reject) => {
    const reader = new FileReader();
    reader.onloadend = () => {
      const result = typeof reader.result === 'string' ? reader.result : '';
      // FileReader produces "data:mime;base64,..." — strip the prefix.
      const comma = result.indexOf(',');
      resolve(comma >= 0 ? result.slice(comma + 1) : result);
    };
    reader.onerror = () => reject(reader.error ?? new Error('Failed to read blob'));
    reader.readAsDataURL(blob);
  });
}
