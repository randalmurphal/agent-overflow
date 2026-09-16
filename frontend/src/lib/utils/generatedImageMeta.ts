/**
 * A picture the agent produced, as the timeline sees it.
 *
 * The backend writes it as an ordinary top-level `assistant_text` row whose
 * meta carries `attachments` (the same shape a user message carries) plus a
 * `generatedImage` record naming the tool row it came from. That is what puts
 * it beside assistant prose rather than on the activity rail, and it is why
 * the tiles, the lightbox, transfer rewriting and export all work on it with
 * no new mechanism — see internal/triage/codex_generated_image.go.
 *
 * `generatedImage.error` is the import having failed: no bytes were published
 * and the reader is shown the reason instead of a broken image.
 */
import { parseUserMessageAttachments } from './userMessageMeta';
import type { AttachmentPreviewSource } from './userMessageMeta';
import { parseJsonObject } from './parseJsonObject';
import type { Item } from '../types/models';
import { imageAttachments } from '../types/attachment';

export interface GeneratedImageRow {
  /** The tool_call row this picture came from. */
  readonly sourceItemId: string;
  /** The model's revised prompt, when it reported one. */
  readonly prompt: string;
  /** Non-empty when the import failed; no images are present then. */
  readonly error: string;
  readonly images: readonly AttachmentPreviewSource[];
}

function stringField(value: unknown): string {
  return typeof value === 'string' ? value.trim() : '';
}

/**
 * The generated-image reading of an item, or null when it is not one.
 *
 * Discriminates on the `generatedImage` meta key, never on the row's text: a
 * row whose prompt happens to read like a caption is still ordinary prose.
 */
export function generatedImageRow(item: Item): GeneratedImageRow | null {
  if (item.kind !== 'assistant_text') return null;
  const record = parseJsonObject(item.meta)?.generatedImage;
  if (!record || typeof record !== 'object' || Array.isArray(record)) return null;
  const fields = record as Record<string, unknown>;
  const images = imageAttachments(parseUserMessageAttachments(item.meta, item.threadId));
  const error = stringField(fields.error);
  // Neither bytes nor a reason is not a row worth rendering specially; fall
  // back to the ordinary assistant renderer rather than showing an empty
  // frame.
  if (images.length === 0 && error === '') return null;
  return {
    sourceItemId: stringField(fields.sourceItemId),
    prompt: stringField(fields.prompt),
    error,
    images,
  };
}
