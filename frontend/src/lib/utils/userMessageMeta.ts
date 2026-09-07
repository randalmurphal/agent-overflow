import {
  DEFAULT_MAX_ATTACHMENT_COUNT,
  isAllowedAttachmentMime,
  maxAttachmentSizeFor,
  type Attachment,
  type AttachmentKind,
} from '../types/attachment';
import type { Item, SourceProposedPlan } from '../types/models';
import { commandWordRanges, type CommandWordRange } from './commandWords';

export interface AttachmentPreviewSource {
  id: string;
  threadId: string;
  filename: string;
  mimeType: string;
  size: number;
  kind: AttachmentKind;
}

export interface UserMessageMeta {
  sendId?: unknown;
  attachments?: unknown;
  sourceProposedPlan?: unknown;
  wire_only?: unknown;
  command?: unknown;
  /**
   * Who produced this message, when it was not the person in front of Agent
   * Overflow. Two values today, and the difference matters to a reader:
   *
   * - `external-queue` — Codex's own `thread/queue`
   *   (`codex queue --thread ...`), which injects a turn into a thread this
   *   app holds. Written by the USER, from another window.
   *   (`internal/provider/codex/external_turns.go#ExternalTurnOriginQueue`)
   * - `peer-session` — another Claude session on this machine addressed this
   *   thread through Claude Code's cross-session inbox. Written by a MODEL.
   *   (`internal/provider/claude/session_peer.go#PeerTurnOrigin`)
   *
   * Absent means locally authored, which is the overwhelmingly common case.
   */
  origin?: unknown;
  /**
   * The peer session's registered display name, on a `peer-session` row.
   * May be absent — an older CLI reports only a socket address — in which
   * case the row is labelled without a name rather than with an empty one.
   */
  cross_session_from_name?: unknown;
}

/**
 * Attribution for a user row Agent Overflow did not originate. Returns the
 * empty string for the ordinary case so callers can branch on truthiness.
 */
export function userMessageOrigin(meta: UserMessageMeta): string {
  return typeof meta.origin === 'string' ? meta.origin.trim() : '';
}

/**
 * The label for a message another Claude session sent to this thread.
 *
 * Names the peer when the wire gave a name, because "from BETA" is what
 * lets the reader go look at BETA. Falls back to the unnamed form rather
 * than to an empty name — a message from nobody reads as a bug.
 */
export function peerSessionOriginLabel(meta: UserMessageMeta): string {
  const name = typeof meta.cross_session_from_name === 'string' ? meta.cross_session_from_name.trim() : '';
  return name ? `From ${name} (another Claude session)` : 'From another Claude session';
}

function stringField(value: unknown): string {
  return typeof value === 'string' ? value.trim() : '';
}

function sizeField(value: unknown, maxBytes: number): number | null {
  if (typeof value !== 'number' || !Number.isFinite(value)) return null;
  if (value < 0 || value > maxBytes) return null;
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
 * True for a top-level user_text row the reader actually wrote. Wire-only
 * rows — context injections the send path marks in meta — fail it, as do
 * subagent-child user rows. The ONE TypeScript counterpart of the store's
 * SQL predicate (items_read.go readerAuthoredUserTextFilter): the nav
 * rail's tick derivation, the sidebar's activity counting, and any future
 * "which user messages are real" question all route here so they cannot
 * drift.
 */
export function isReaderAuthoredUserText(item: Item): boolean {
  if (item.kind !== 'user_text') return false;
  if ((item.parentId ?? '') !== '') return false;
  return parseUserMessageMeta(item.meta).wire_only !== true;
}

/**
 * Strip the attachment-image blocks the send path appends to a user
 * message's summary. Shared by UserMessage's rendering and the nav
 * rail's preview so the two cannot disagree about the href shape.
 */
export function stripAttachmentImages(summary: string): string {
  return summary.replace(/\n\n!\[[^\]]*]\(attachment:\/\/[^\s)]+\)/g, '');
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

  // Absent means IMAGE: every row written before the kind existed carried
  // one. Only the literal `file` is a file, so an unrecognised value stays on
  // the strict branch and has to pass the image MIME check.
  const kind: AttachmentKind = stringField(record.kind) === 'file' ? 'file' : 'image';

  const mimeType = stringField(record.mimeType);
  // A file's MIME is whatever the browser declared and the backend bounded;
  // it is never decoded here, so there is nothing to validate it against.
  if (kind === 'image' && !isAllowedAttachmentMime(mimeType)) return null;

  const size = sizeField(record.size, maxAttachmentSizeFor(kind));
  if (size === null) return null;

  return {
    id,
    threadId,
    filename: stringField(record.filename) || id,
    mimeType,
    size,
    kind,
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
