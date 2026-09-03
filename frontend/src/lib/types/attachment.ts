/**
 * An attachment is one of two KINDS, and the kind is what every layer
 * switches on (`internal/attachment` decides it server-side, from the
 * declared MIME and the filename, and it is the row's durable answer):
 *
 * - `image` — a provider-ingestible image. It is bound positionally to an
 *   `[Image #N]` marker in the message text, N counting IMAGES only, and it
 *   is the only kind whose bytes are ever served back (thumbnail, lightbox).
 * - `file` — everything else. It never gets textarea text, never has a
 *   preview requested for it, and is removed only through its own chip; the
 *   backend appends its path to the provider payload at send time.
 */
export type AttachmentKind = 'image' | 'file';

export interface Attachment {
  id: string;
  threadId: string;
  filename: string;
  mimeType: string;
  size: number;
  relativePath: string;
  createdAt: number;
  kind: AttachmentKind;
}

/** Accepted image MIME types for composer attachments. */
export const ALLOWED_ATTACHMENT_MIME_TYPES = new Set<string>([
  'image/png',
  'image/jpeg',
  'image/jpg',
  'image/webp',
  'image/gif',
]);

/** Image ceiling, mirroring `attachment.DefaultMaxSize`. */
export const DEFAULT_MAX_ATTACHMENT_SIZE = 10 * 1024 * 1024;
/**
 * File ceiling, mirroring `attachment.DefaultMaxFileSize`. Larger than the
 * image cap because a file is referenced by path rather than re-encoded into
 * the model's context.
 */
export const DEFAULT_MAX_FILE_ATTACHMENT_SIZE = 50 * 1024 * 1024;
export const DEFAULT_MAX_ATTACHMENT_COUNT = 8;

export function isAllowedAttachmentMime(mime: string | undefined | null): boolean {
  if (!mime) return false;
  return ALLOWED_ATTACHMENT_MIME_TYPES.has(mime.toLowerCase());
}

/** The per-kind size ceiling. */
export function maxAttachmentSizeFor(kind: AttachmentKind): number {
  return kind === 'file' ? DEFAULT_MAX_FILE_ATTACHMENT_SIZE : DEFAULT_MAX_ATTACHMENT_SIZE;
}

/**
 * The image subset, in order. THE one place the kind is filtered on: image
 * placeholders number images, previews load images, and both attachment rows
 * render images as tiles — so all of them ask here rather than restating the
 * predicate and drifting apart.
 */
export function imageAttachments<T extends { kind: AttachmentKind }>(attachments: T[]): T[] {
  return attachments.filter((attachment) => attachment.kind !== 'file');
}

/** Mirrors `attachment.FormatSize`, so the size the user saw is the size the agent is told. */
export function formatAttachmentSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}
