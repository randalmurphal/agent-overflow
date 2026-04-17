import { describe, expect, it } from 'vitest';
import { blobToBase64 } from './captureHtml';

// captureHtmlToPng itself needs a real layout engine (modern-screenshot +
// an iframe that actually loads) which happy-dom can't simulate. The
// export-flow tests in DesignPreviewPanel.test.ts mock it at the module
// boundary. What we CAN cover in pure isolation:

describe('blobToBase64', () => {
  it('round-trips ASCII bytes through the base64 encoding', async () => {
    const blob = new Blob(['hello'], { type: 'text/plain' });
    const b64 = await blobToBase64(blob);
    // "hello" → "aGVsbG8=" in base64. The helper strips the data-URL
    // prefix so callers can feed it straight into UploadAttachment.
    expect(b64).toBe('aGVsbG8=');
  });

  it('handles empty blobs without hanging', async () => {
    const blob = new Blob([], { type: 'application/octet-stream' });
    const b64 = await blobToBase64(blob);
    expect(b64).toBe('');
  });

  it('preserves binary payloads', async () => {
    const bytes = new Uint8Array([0, 1, 2, 3, 255, 254, 253]);
    const blob = new Blob([bytes], { type: 'application/octet-stream' });
    const b64 = await blobToBase64(blob);
    // base64(AAECA//+/Q==) is the canonical encoding of these bytes.
    expect(b64).toBe('AAECA//+/Q==');
  });
});
