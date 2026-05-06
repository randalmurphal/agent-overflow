// Capture the contents of the design-mode iframe. The iframe is
// loaded with `sandbox="allow-scripts"` (no `allow-same-origin`), so
// the parent CANNOT reach into `iframe.contentDocument` to render the
// DOM directly. Instead we round-trip through postMessage: the file
// server injects a capture script into every served HTML document;
// that script lazy-loads modern-screenshot inside the iframe and
// posts the result back. The parent here is just the messenger.
//
// Outbound envelope:
//
//   parent → iframe: { aoDesign: 'capture', requestId, mode: 'single' }
//
// Reply shapes:
//
//   { aoDesign: 'capture-result', requestId, pngBase64 }
//   { aoDesign: 'capture-error',  requestId, error }
//
// The agent's read_screenshot MCP tool does NOT use this rail — that
// path is backend-driven (chromedp / chrome-headless-shell renders
// the same /design/ URL). This helper is exclusively the user-facing
// "send to thread" capture: one PNG attached to one chat draft.

const DEFAULT_CAPTURE_TIMEOUT_MS = 5000;

interface CaptureResultMessage {
  aoDesign: 'capture-result';
  requestId: string;
  pngBase64: string;
}

interface CaptureErrorMessage {
  aoDesign: 'capture-error';
  requestId: string;
  error: string;
}

type CaptureMessage = CaptureResultMessage | CaptureErrorMessage;

function isCaptureMessage(value: unknown): value is CaptureMessage {
  if (!value || typeof value !== 'object') return false;
  const v = value as Record<string, unknown>;
  return (
    (v.aoDesign === 'capture-result' || v.aoDesign === 'capture-error')
    && typeof v.requestId === 'string'
  );
}

/**
 * Request a single full-document PNG screenshot of the live design
 * iframe. Returns the raw base64-encoded PNG payload (no
 * `data:image/png;base64,` prefix).
 *
 * Used by the user-facing "send to thread" attachment flow.
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
