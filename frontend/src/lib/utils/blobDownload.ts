// A browser download of bytes this page holds, shared by the image menu
// (`imageMenuActions.ts`) and the forge attachment activation
// (`forgeAttachmentActions.ts`).

/**
 * How long a download's object URL outlives the click. The anchor click
 * only starts the download; some engines read the URL later, so revoking
 * synchronously can cancel it. Bounded, so the bytes are not held for the
 * page's lifetime.
 */
export const DOWNLOAD_URL_LIFETIME_MS = 40_000;

/** Download `blob` under `filename` through a transient anchor. */
export function downloadBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement('a');
  anchor.href = url;
  anchor.download = filename;
  anchor.rel = 'noopener';
  anchor.style.display = 'none';
  document.body.appendChild(anchor);
  try {
    anchor.click();
  } finally {
    anchor.remove();
    setTimeout(() => URL.revokeObjectURL(url), DOWNLOAD_URL_LIFETIME_MS);
  }
}
