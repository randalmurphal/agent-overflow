// Pure helpers for the composer's attachment flow. Extracted from
// Composer.svelte so the parent stays focused on message entry.
// These functions are unit-tested in `attachmentHelpers.test.ts`
// without mounting the whole composer.

import {
  isAllowedAttachmentMime,
  maxAttachmentSizeFor,
  type AttachmentKind,
} from '../../types/attachment';

export {
  DEFAULT_MAX_ATTACHMENT_COUNT,
  DEFAULT_MAX_ATTACHMENT_SIZE,
} from '../../types/attachment';

/**
 * Sniff common image extensions so a file that lost its MIME type
 * during drag/drop can still pass the upload guard.
 */
export function matchesImageExtension(name: string): boolean {
  return /\.(png|jpe?g|gif|webp)$/i.test(name);
}

/**
 * Which kind an upload will become, decided the way the backend decides it
 * (`attachment.classifyUpload`): a declared image MIME or an image extension
 * makes it an image, and everything else is a file at face value. Nothing is
 * ever reclassified INTO an image, so a payload that claims to be one and is
 * not is refused server-side rather than demoted here.
 */
export function classifyAttachment(mimeType: string, filename: string): AttachmentKind {
  return isAllowedAttachmentMime(mimeType) || matchesImageExtension(filename) ? 'image' : 'file';
}

/**
 * Returns a short human-readable rejection reason when a file should
 * not be uploaded, or null when it passes the guard. Centralising the
 * checks keeps the "why did my upload not happen?" messaging consistent
 * across drag/drop, paste, and the attachment row.
 *
 * No type is rejected any more — an unrecognised one is a `file`. What is
 * left is the per-kind size ceiling, and the message names the kind because
 * there are now two limits and "limit is 10 MB" would read as a lie beside a
 * 40 MB PDF that was fine.
 *
 * The image ceiling is the caller's, because it is also what recompression
 * targets; the file ceiling has no second consumer and comes from the policy
 * constant.
 */
export function rejectionReason(file: File, maxImageBytes: number): string | null {
  const kind = classifyAttachment(file.type, file.name);
  const maxBytes = kind === 'image' ? maxImageBytes : maxAttachmentSizeFor(kind);
  if (file.size > maxBytes) {
    const mb = (file.size / 1024 / 1024).toFixed(1);
    const limit = Math.round(maxBytes / 1024 / 1024);
    return `${file.name} is ${mb} MB; limit for ${kind}s is ${limit} MB`;
  }
  return null;
}
