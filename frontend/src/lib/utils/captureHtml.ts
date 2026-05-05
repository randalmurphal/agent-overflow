// Capture the contents of the design-mode iframe as a PNG. The iframe
// is loaded with `sandbox="allow-scripts"` (no `allow-same-origin`),
// which means the parent CANNOT reach into `iframe.contentDocument` to
// render the DOM directly. Instead we round-trip through postMessage:
// the file server injects a capture script into every served HTML
// document; that script lazy-loads modern-screenshot inside the iframe,
// renders document.documentElement, and posts the PNG bytes back over
// postMessage. The parent here is just the messenger.
//
// The wire shape is:
//   parent → iframe: { aoDesign: 'capture', requestId }
//   iframe → parent: { aoDesign: 'capture-result', requestId, pngBase64 }
//                or: { aoDesign: 'capture-error',  requestId, error }
//
// Resolves with the raw (un-prefixed) base64 payload IngestScreenshot
// expects. Rejects on capture-error, missing iframe contentWindow, or
// timeout.

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
    (v.aoDesign === 'capture-result' || v.aoDesign === 'capture-error') &&
    typeof v.requestId === 'string'
  );
}

/**
 * Request a screenshot of the live design iframe. Returns the raw
 * base64-encoded PNG payload (no `data:image/png;base64,` prefix).
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
    let settled = false;

    function onMessage(ev: MessageEvent) {
      // Cross-origin: ev.source is the iframe's window, but
      // contentWindow access for comparison is fine even with strict
      // sandbox because we already have a reference. Match by
      // requestId — the iframe might have other listeners running.
      if (ev.source !== win) return;
      const data = ev.data;
      if (!isCaptureMessage(data)) return;
      if (data.requestId !== captureRequestId) return;

      cleanup();
      if (data.aoDesign === 'capture-error') {
        reject(new Error(data.error || 'capture failed'));
        return;
      }
      resolve(data.pngBase64 || '');
    }

    function cleanup() {
      if (settled) return;
      settled = true;
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
        { aoDesign: 'capture', requestId: captureRequestId },
        '*',
      );
    } catch (err) {
      cleanup();
      reject(err instanceof Error ? err : new Error(String(err)));
    }
  });
}
