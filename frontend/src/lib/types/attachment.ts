export interface Attachment {
  id: string;
  threadId: string;
  filename: string;
  mimeType: string;
  size: number;
  relativePath: string;
  createdAt: number;
}

export interface PendingAttachment {
  id: string;
  filename: string;
  mimeType: string;
  size: number;
  dataUrl: string;
}

/** Accepted image MIME types for composer attachments. */
export const ALLOWED_ATTACHMENT_MIME_TYPES = new Set<string>([
  'image/png',
  'image/jpeg',
  'image/jpg',
  'image/webp',
  'image/gif',
]);

export const DEFAULT_MAX_ATTACHMENT_SIZE = 10 * 1024 * 1024;
export const DEFAULT_MAX_ATTACHMENT_COUNT = 8;

export function isAllowedAttachmentMime(mime: string | undefined | null): boolean {
  if (!mime) return false;
  return ALLOWED_ATTACHMENT_MIME_TYPES.has(mime.toLowerCase());
}

export function formatAttachmentSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}
