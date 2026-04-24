// Pure helpers for the composer's attachment flow. Extracted from
// Composer.svelte so the parent stays focused on message entry.
// These functions are unit-tested in `attachmentHelpers.test.ts`
// without mounting the whole composer.

import {
  DEFAULT_MAX_ATTACHMENT_SIZE,
  isAllowedAttachmentMime,
} from '../../types/attachment';

export {
  DEFAULT_MAX_ATTACHMENT_COUNT,
  DEFAULT_MAX_ATTACHMENT_SIZE,
} from '../../types/attachment';

/**
 * Return the non-null data URL payload as base64 (without the
 * `data:mime/type;base64,` prefix). FileReader is promise-wrapped so
 * the upload flow can `await` cleanly.
 */
export function fileToBase64(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onerror = () => reject(new Error('Failed to read file'));
    reader.onload = () => {
      const result = reader.result;
      if (typeof result !== 'string') {
        reject(new Error('Unexpected reader result'));
        return;
      }
      const commaIdx = result.indexOf(',');
      resolve(commaIdx >= 0 ? result.slice(commaIdx + 1) : result);
    };
    reader.readAsDataURL(file);
  });
}

/**
 * Sniff common image extensions so a file that lost its MIME type
 * during drag/drop can still pass the upload guard.
 */
export function matchesImageExtension(name: string): boolean {
  return /\.(png|jpe?g|gif|webp)$/i.test(name);
}

/**
 * Returns a short human-readable rejection reason when a file should
 * not be uploaded, or null when it passes the guard. Centralising the
 * checks keeps the "why did my upload not happen?" messaging consistent
 * across drag/drop, paste, and the attachment row.
 */
export function rejectionReason(file: File, maxBytes: number): string | null {
  if (!isAllowedAttachmentMime(file.type) && !matchesImageExtension(file.name)) {
    return `Unsupported file type: ${file.name}`;
  }
  if (file.size > maxBytes) {
    const mb = (file.size / 1024 / 1024).toFixed(1);
    const limit = Math.round(maxBytes / 1024 / 1024);
    return `${file.name} is ${mb} MB; limit is ${limit} MB`;
  }
  return null;
}

/**
 * True when the DragEvent is carrying file payloads (or an image MIME
 * type) we care about. Used to ignore noise from the many non-file
 * drag sources the browser fires (text, links, etc.).
 */
export function hasImagePayload(event: DragEvent): boolean {
  const types = event.dataTransfer?.types;
  if (!types) return false;
  return Array.from(types).some((type) => type === 'Files' || type.startsWith('image/'));
}

/**
 * Extract image files from a ClipboardEvent. Returns the non-empty
 * list or an empty array if no image clippings exist.
 */
export function extractClipboardImages(event: ClipboardEvent): File[] {
  const clip = event.clipboardData;
  if (!clip) return [];
  const files: File[] = [];
  for (const item of Array.from(clip.items)) {
    if (item.kind === 'file' && item.type.startsWith('image/')) {
      const file = item.getAsFile();
      if (file) files.push(file);
    }
  }
  return files;
}
