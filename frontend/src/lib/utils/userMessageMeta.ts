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
  joinedSendIds?: unknown;
  pendingFlush?: unknown;
  attachments?: unknown;
  sourceProposedPlan?: unknown;
  wire_only?: unknown;
  command?: unknown;
  /** Set by the composer path only; see isAgentMessageMeta. */
  expandComposerCommands?: unknown;
  /**
   * Who produced this message, when it was not the person in front of Agent
   * Overflow. The difference matters to a reader:
   *
   * - `external-queue` — Codex's own `thread/queue`
   *   (`codex queue --thread ...`), which injects a turn into a thread this
   *   app holds. Written by the USER, from another window.
   *   (`internal/provider/codex/external_turns.go#ExternalTurnOriginQueue`)
   * - `peer-session` — another Claude session on this machine addressed this
   *   thread through Claude Code's cross-session inbox. Written by a MODEL.
   *   (`internal/provider/claude/session_peer.go#PeerTurnOrigin`)
   * - `agent-thread` — an agent in another Agent Overflow thread, on this
   *   computer or on a paired one, wrote this row through the thread tools:
   *   a spawn's first message, a `thread_send`, a `thread_ask`, or the wake
   *   carrying an answer back. `originThread` names the source thread. It
   *   renders as an agent-message row, not the user bubble
   *   (`docs/specs/agent-thread-tools.md`, How agent messages appear).
   * - `scheduled` — the prompt of a turn the provider started on its own
   *   schedule: a Claude cron job's fire or ScheduleWakeup wake. The model
   *   stored it earlier; nobody typed it now.
   *   (`internal/provider/claude/parse_turn_origin.go#ScheduledTurnOrigin`)
   * - `agent-wake` — the wake supervisor wrote this row: a loop's fire, a
   *   watch's output, or a script's `agent-overflow thread send`. Data
   *   delivered to the thread, never the user speaking. `originWake` names
   *   the loop or watch. It renders as an agent-message row too.
   *   (`docs/specs/standing-agents.md`, Wake supervisor)
   *
   * Absent means locally authored, which is the overwhelmingly common case.
   */
  origin?: unknown;
  /** The source thread of an `agent-thread` row. See userMessageOriginThread. */
  originThread?: unknown;
  /** What wrote an `agent-wake` row. See userMessageOriginWake. */
  originWake?: unknown;
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
 * The thread an `agent-thread` row came from, as the tools stamped it.
 *
 * `threadId` is the whole point of the chip — it is what the reader clicks
 * through to — so a value without one is not attribution and is refused.
 * Everything else is best effort: a missing title still renders a chip, and
 * an absent computer means the row was written on the computer showing it.
 */
export interface UserMessageOriginThread {
  computerId: string;
  computerName: string;
  threadId: string;
  title: string;
  /** The request token, so three requests from one thread stay distinct. */
  token: string;
  /**
   * What the row delivers: `spawn`, `send` or `ask` on a request, `wake` on
   * an answer coming back. Empty on rows written before it was stamped.
   */
  kind: string;
}

export function userMessageOriginThread(meta: UserMessageMeta): UserMessageOriginThread | null {
  const record = objectRecord(meta.originThread);
  if (!record) return null;
  const threadId = stringField(record.threadId);
  if (!threadId) return null;
  return {
    computerId: stringField(record.computerId),
    computerName: stringField(record.computerName),
    threadId,
    title: stringField(record.title),
    token: stringField(record.token),
    kind: stringField(record.kind),
  };
}

/**
 * What wrote an `agent-wake` row: `loop` or `watch` with its label, `send`
 * for a script's outside message, or `question` or `approval` for a worker's
 * prompt put to the standing agent that spawned it, labelled with the
 * worker's title. A row without a known kind is not attribution and is
 * refused.
 */
export interface UserMessageOriginWake {
  kind: 'loop' | 'watch' | 'send' | 'question' | 'approval';
  label: string;
  /** The worker of a `question` or `approval` wake; empty otherwise. */
  threadId: string;
}

const WAKE_KINDS: ReadonlySet<string> = new Set(['loop', 'watch', 'send', 'question', 'approval']);
const WAKE_KIND_LABELS: Record<Exclude<UserMessageOriginWake['kind'], 'send'>, string> = {
  loop: 'Loop',
  watch: 'Watch',
  question: 'Question',
  approval: 'Approval',
};

export function userMessageOriginWake(meta: UserMessageMeta): UserMessageOriginWake | null {
  const record = objectRecord(meta.originWake);
  if (!record) return null;
  const kind = stringField(record.kind);
  if (!WAKE_KINDS.has(kind)) return null;
  return {
    kind: kind as UserMessageOriginWake['kind'],
    label: stringField(record.label),
    threadId: stringField(record.threadId),
  };
}

/** The badge a wake row carries: which loop, watch or worker, or that a script sent it. */
export function agentWakeOriginLabel(wake: UserMessageOriginWake): string {
  if (wake.kind === 'send') return 'Sent by a script';
  const kind = WAKE_KIND_LABELS[wake.kind];
  return wake.label ? `${kind} · ${wake.label}` : kind;
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

/** A quiet stored copy, before provider consumption or interrupt promotion. */
export function isPendingFlushRow(item: Item): boolean {
  return item.kind === 'user_text' && parseUserMessageMeta(item.meta).pendingFlush === true;
}

/**
 * True for a message an agent wrote into this thread, not the person: an
 * `agent-thread` or `agent-wake` row. A row that also carries the composer's
 * `expandComposerCommands` is the person's: headless Claude joins queued
 * messages at a turn boundary, so one row can hold the person's message
 * after an agent's, attributed to the agent. The Go twin is
 * usermessage.IsAgentMessage; the SQL one store.AgentMessageFilterFor.
 */
export function isAgentMessageMeta(meta: UserMessageMeta): boolean {
  if (meta.expandComposerCommands === true) return false;
  const origin = userMessageOrigin(meta);
  return origin === 'agent-thread' || origin === 'agent-wake';
}

/**
 * True for a top-level user_text row the reader actually wrote. Wire-only
 * rows (context injections the send path marks in meta) fail it, as do
 * subagent-child user rows and an agent's messages (isAgentMessageMeta).
 * The ONE TypeScript counterpart of the store's tick predicate
 * (items_read.go personMessageFilterFor): the nav rail's tick derivation,
 * the composer's recall, the un-send on interrupt, and any future "which
 * user messages are the person's" question all route here so they cannot
 * drift.
 */
export function isReaderAuthoredUserText(item: Item): boolean {
  if (item.kind !== 'user_text') return false;
  if ((item.parentId ?? '') !== '') return false;
  const meta = parseUserMessageMeta(item.meta);
  return meta.wire_only !== true && !isAgentMessageMeta(meta);
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
  // Forks retain attachment provenance in metadata. Access is authorized
  // through the viewing thread's ownership, even after the original is gone.
  const threadId = expectedThreadId || metaThreadId;
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
