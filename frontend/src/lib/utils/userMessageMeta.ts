import {
  DEFAULT_MAX_ATTACHMENT_COUNT,
  DEFAULT_MAX_ATTACHMENT_SIZE,
  isAllowedAttachmentMime,
  type Attachment,
} from '../types/attachment';
import type { Item, SourceProposedPlan } from '../types/models';
import { commandWordRanges, type CommandWordRange } from './commandWords';

export interface AttachmentPreviewSource {
  id: string;
  threadId: string;
  filename: string;
  mimeType: string;
  size: number;
}

export interface UserMessageMeta {
  attachments?: unknown;
  sourceProposedPlan?: unknown;
  wire_only?: unknown;
  command?: unknown;
}

function stringField(value: unknown): string {
  return typeof value === 'string' ? value.trim() : '';
}

function sizeField(value: unknown): number | null {
  if (typeof value !== 'number' || !Number.isFinite(value)) return null;
  if (value < 0 || value > DEFAULT_MAX_ATTACHMENT_SIZE) return null;
  return value;
}

function objectRecord(value: unknown): Record<string, unknown> | null {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null;
  return value as Record<string, unknown>;
}

export function parseUserMessageMeta(meta: string | undefined): UserMessageMeta {
  if (!meta) return {};
  try {
    const parsed = JSON.parse(meta);
    return objectRecord(parsed) ?? {};
  } catch {
    return {};
  }
}

/**
 * Where to colour command words on a persisted user row.
 *
 * Keyed on the meta marker the send path wrote (D31), NOT on a live registry
 * match: history must say what actually expanded, so a command removed from
 * the registry keeps its colour on the rows it really ran on, and a word that
 * merely looks like a command on an old row never gains one. The word still
 * has to be there — a row whose text no longer contains the marked command
 * (a revert-rehydrated draft the user edited, say) renders plain rather than
 * colouring something that means nothing.
 *
 * Every occurrence is returned, at any word position, exactly as the composer
 * painted them while the message was being typed. The expansion still happened
 * once; the colour is about which words were live, not how many blocks were
 * appended.
 */
export function userMessageCommandRanges(
  meta: string | undefined,
  summary: string,
): CommandWordRange[] {
  const command = parseUserMessageMeta(meta).command;
  if (typeof command !== 'string' || command === '') return [];
  return commandWordRanges(summary).filter((range) => range.name === command);
}

export function sourceProposedPlanFromUserMessageMeta(value: unknown): SourceProposedPlan | null {
  const record = objectRecord(value);
  if (!record) return null;
  const itemId = stringField(record.itemId);
  if (!itemId) return null;
  const source: SourceProposedPlan = { itemId };
  const threadId = stringField(record.threadId);
  const payloadId = stringField(record.payloadId);
  const title = stringField(record.title);
  if (threadId) source.threadId = threadId;
  if (payloadId) source.payloadId = payloadId;
  if (title) source.title = title;
  return source;
}

function attachmentFromMeta(
  value: unknown,
  expectedThreadId?: string,
): AttachmentPreviewSource | null {
  const record = objectRecord(value);
  if (!record) return null;
  const id = stringField(record.id);
  if (!id) return null;

  const metaThreadId = stringField(record.threadId);
  if (expectedThreadId && metaThreadId && metaThreadId !== expectedThreadId) return null;
  const threadId = metaThreadId || expectedThreadId;
  if (!threadId) return null;

  const mimeType = stringField(record.mimeType);
  if (!isAllowedAttachmentMime(mimeType)) return null;

  const size = sizeField(record.size);
  if (size === null) return null;

  return {
    id,
    threadId,
    filename: stringField(record.filename) || id,
    mimeType,
    size,
  };
}

export function parseUserMessageAttachments(
  meta: string | undefined,
  expectedThreadId?: string,
): AttachmentPreviewSource[] {
  const parsed = parseUserMessageMeta(meta);
  if (!Array.isArray(parsed.attachments)) return [];
  const attachments: AttachmentPreviewSource[] = [];
  for (const attachment of parsed.attachments) {
    if (attachments.length >= DEFAULT_MAX_ATTACHMENT_COUNT) break;
    const normalized = attachmentFromMeta(attachment, expectedThreadId);
    if (normalized) attachments.push(normalized);
  }
  return attachments;
}

export function restoredDraftSnapshotAttachmentsFromUserItem(item: Item): Attachment[] {
  return parseUserMessageAttachments(item.meta, item.threadId).map((attachment) => ({
    ...attachment,
    relativePath: '',
    createdAt: item.createdAt,
  }));
}
