// Capture the contents of the design-mode iframe. The iframe is
// loaded with `sandbox="allow-scripts"` (no `allow-same-origin`), so
// the parent CANNOT reach into `iframe.contentDocument` to render the
// DOM directly. Instead we round-trip through postMessage: the file
// server injects a capture script into every served HTML document;
// that script lazy-loads modern-screenshot inside the iframe and
// posts the result back. The parent here is just the messenger.
//
// Two modes share the same outbound envelope:
//
//   parent → iframe: { aoDesign: 'capture', requestId, mode? }
//
// `mode` defaults to 'single' on the iframe side. Reply shapes differ
// per mode:
//
//   single (used by the user-facing "send to thread" upload — one PNG
//   over the wire becomes one chat attachment):
//     { aoDesign: 'capture-result', requestId, pngBase64 }
//
//   tiles (used by the agent's read_screenshot MCP tool — tiling keeps
//   each image inside per-image vision-token budgets):
//     { aoDesign: 'capture-tiles-result', requestId, tilesJpegBase64,
//       clipped }
//
// Both modes can fail with:
//   { aoDesign: 'capture-error', requestId, error }
//
// Each helper resolves with raw (un-prefixed) base64 payloads and
// rejects on capture-error, missing iframe contentWindow, or timeout.

const DEFAULT_CAPTURE_TIMEOUT_MS = 5000;

interface CaptureResultMessage {
  aoDesign: 'capture-result';
  requestId: string;
  pngBase64: string;
}

interface CaptureTilesResultMessage {
  aoDesign: 'capture-tiles-result';
  requestId: string;
  tilesJpegBase64: string[];
  clipped?: boolean;
}

interface CaptureErrorMessage {
  aoDesign: 'capture-error';
  requestId: string;
  error: string;
}

type CaptureMessage =
  | CaptureResultMessage
  | CaptureTilesResultMessage
  | CaptureErrorMessage;

function isCaptureMessage(value: unknown): value is CaptureMessage {
  if (!value || typeof value !== 'object') return false;
  const v = value as Record<string, unknown>;
  return (
    (v.aoDesign === 'capture-result'
      || v.aoDesign === 'capture-tiles-result'
      || v.aoDesign === 'capture-error')
    && typeof v.requestId === 'string'
  );
}

/**
 * Request a single full-document PNG screenshot of the live design
 * iframe. Returns the raw base64-encoded PNG payload (no
 * `data:image/png;base64,` prefix).
 *
 * Used by the user-facing "send to thread" attachment flow. The agent
 * path uses {@link requestIframeCaptureTiles} instead — the single PNG
 * is too large for per-tool-call vision budgets on tall pages.
 *
 * The captureRequestId pairs the response with the parent's request so
 * a reload in flight doesn't deliver someone else's bytes.
 */
export async function requestIframeCapture(
  iframe: HTMLIFrameElement,
  captureRequestId: string,
  timeoutMs: number = DEFAULT_CAPTURE_TIMEOUT_MS,
): Promise<string> {
  const win = iframe.contentWindow;
  if (!win) {
    throw new Error('Capture target iframe has no contentWindow.');
  }
  return new Promise<string>((resolve, reject) => {
    function onMessage(ev: MessageEvent) {
      if (ev.source !== win) return;
      const data = ev.data;
      if (!isCaptureMessage(data)) return;
      if (data.requestId !== captureRequestId) return;

      cleanup();
      if (data.aoDesign === 'capture-error') {
        reject(new Error(data.error || 'capture failed'));
        return;
      }
      if (data.aoDesign !== 'capture-result') {
        reject(new Error(`unexpected capture result kind: ${data.aoDesign}`));
        return;
      }
      resolve(data.pngBase64 || '');
    }

    function cleanup() {
      window.removeEventListener('message', onMessage);
      clearTimeout(timer);
    }

    const timer = setTimeout(() => {
      cleanup();
      reject(new Error(`capture timed out after ${timeoutMs}ms`));
    }, timeoutMs);

    window.addEventListener('message', onMessage);
    try {
      win.postMessage(
        { aoDesign: 'capture', requestId: captureRequestId, mode: 'single' },
        '*',
      );
    } catch (err) {
      cleanup();
      reject(err instanceof Error ? err : new Error(String(err)));
    }
  });
}

/**
 * Result of a tiled capture: ordered base64-encoded JPEG tiles
 * top-to-bottom plus a clipped flag set when the rendered document
 * exceeded the iframe-side tile budget.
 */
export interface IframeCaptureTilesResult {
  tiles: string[];
  clipped: boolean;
}

/**
 * Request a tiled JPEG screenshot of the live design iframe. Each
 * tile is the raw base64-encoded JPEG payload (no
 * `data:image/jpeg;base64,` prefix). Tiles are ordered top-to-bottom
 * and capped on the iframe side at the configured budget; `clipped`
 * is true when trailing tiles were dropped.
 *
 * Used by the agent's read_screenshot MCP tool. Each tile maps 1:1
 * to an image content block in the tool result.
 */
export async function requestIframeCaptureTiles(
  iframe: HTMLIFrameElement,
  captureRequestId: string,
  timeoutMs: number = DEFAULT_CAPTURE_TIMEOUT_MS,
): Promise<IframeCaptureTilesResult> {
  const win = iframe.contentWindow;
  if (!win) {
    throw new Error('Capture target iframe has no contentWindow.');
  }
  return new Promise<IframeCaptureTilesResult>((resolve, reject) => {
    function onMessage(ev: MessageEvent) {
      if (ev.source !== win) return;
      const data = ev.data;
      if (!isCaptureMessage(data)) return;
      if (data.requestId !== captureRequestId) return;

      cleanup();
      if (data.aoDesign === 'capture-error') {
        reject(new Error(data.error || 'capture failed'));
        return;
      }
      if (data.aoDesign !== 'capture-tiles-result') {
        reject(new Error(`unexpected capture result kind: ${data.aoDesign}`));
        return;
      }
      const tiles = Array.isArray(data.tilesJpegBase64) ? data.tilesJpegBase64 : [];
      if (tiles.length === 0) {
        reject(new Error('capture returned zero tiles'));
        return;
      }
      resolve({ tiles, clipped: data.clipped === true });
    }

    function cleanup() {
      window.removeEventListener('message', onMessage);
      clearTimeout(timer);
    }

    const timer = setTimeout(() => {
      cleanup();
      reject(new Error(`capture timed out after ${timeoutMs}ms`));
    }, timeoutMs);

    window.addEventListener('message', onMessage);
    try {
      win.postMessage(
        { aoDesign: 'capture', requestId: captureRequestId, mode: 'tiles' },
        '*',
      );
    } catch (err) {
      cleanup();
      reject(err instanceof Error ? err : new Error(String(err)));
    }
  });
}
