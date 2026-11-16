import {
  DEFAULT_MAX_ATTACHMENT_COUNT,
  isAllowedAttachmentMime,
  maxAttachmentSizeFor,
  type Attachment,
  type AttachmentKind,
} from '../types/attachment';
import type { Item, SourceProposedPlan } from '../types/models';
import { commandWordRanges, type CommandWordRange } from './commandWords';
import type { ShellOutputMeta } from './shellInvocation';

export interface AttachmentPreviewSource {
  id: string;
  threadId: string;
  filename: string;
  mimeType: string;
  size: number;
  kind: AttachmentKind;
  /** Set when the file is the output of a composer shell command. */
  shell?: ShellOutputMeta;
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
   *   carrying answers back. `originThread` names the source thread; a wake
   *   that lists several threads names the first and its facts name them
   *   all (userMessageFacts). It renders as an agent-message row, not the user bubble
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
   * - `codex-delegation` — another Codex thread started or messaged this one
   *   through Codex's own `create_thread` / `send_message_to_thread` tools,
   *   outside Agent Overflow. Only imported history carries it.
   *   (`internal/provider/codex/rollout/delegation.go`)
   * - `app` — Agent Overflow itself wrote the row, such as the "Continue"
   *   sent after a usage limit (`docs/specs/account-auto-switch.md`).
   *
   * Absent means locally authored, which is the overwhelmingly common case.
   */
  origin?: unknown;
  /** The source thread of an `agent-thread` row. See userMessageOriginThread. */
  originThread?: unknown;
  /** What wrote an `agent-wake` row. See userMessageOriginWake. */
  originWake?: unknown;
  /**
   * A joined row's members in its text, each `{sendId, bytes, origin,
   * originThread, originWake}`: bytes is the member's UTF-8 length, and an
   * agent thread's member carries its request token. `originThread` keeps
   * only the first member's. See userMessageOriginTokens.
   */
  joinedParts?: unknown;
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
 * The request tokens a user row answers: every joined member's (whatever
 * the row's attribution, since a wake may have joined first), read from its
 * parts; else an `agent-thread` row's own. A member's are every request its
 * facts say it answers, a digest's included, and its token when it records
 * none.
 */
export function userMessageOriginTokens(meta: UserMessageMeta): string[] {
  const own = (member: UserMessageMeta): string[] => {
    const facts = userMessageFacts(member);
    if (facts && facts.tokens.length > 0) return facts.tokens;
    const token = userMessageOriginThread(member)?.token;
    return token ? [token] : [];
  };
  if (Array.isArray(meta.joinedParts)) {
    const joined = new Set<string>();
    for (const part of meta.joinedParts as unknown[]) {
      const record = objectRecord(part);
      if (record && stringField(record.origin) === 'agent-thread') for (const token of own(record)) joined.add(token);
    }
    if (joined.size > 0) return [...joined];
  }
  return userMessageOrigin(meta) === 'agent-thread' ? own(meta) : [];
}

/** A range of a message's text in UTF-8 bytes, end exclusive. */
export interface MessageSpan {
  start: number;
  end: number;
}

/** One thread a digest answer speaks for (threadtools FactsThread). */
export interface MessageFactsThread {
  threadId: string;
  title: string;
  /** The thread's computer, '' for the caller's own: the id the row opens it through and the name it reads as. */
  computerId: string;
  computerName: string;
  age: string;
  tokens: string[];
  role: string | null;
}

/**
 * What a message the app wrote for an agent says, as its writer stored it
 * beside the text (threadtools.MessageFacts on `originThread.facts` or
 * `originWake.facts`). A row renders from these, never from the text, which
 * is written for the agent.
 */
export interface MessageFacts {
  /** What an answer says happened (a wake kind, `settled` or `worker-stopped`); '' on a request or a wake. */
  outcome: string;
  /** Where the message's own words are in its text. */
  body: MessageSpan[];
  /** The row's header line when it is not the body's first line. */
  headline: string;
  waiting: boolean;
  reachable: boolean;
  userMessage: string;
  routesPrompts: boolean;
  /** The sender's or answering thread's role when it is a standing agent ('' for none); null when it is not one. */
  role: string | null;
  tokens: string[];
  age: string;
  stoppedBy: string;
  clipped: boolean;
  madeIn: string;
  now: string;
  /** A digest's threads, two or more. */
  threads: MessageFactsThread[];
}

/**
 * The facts a row's attribution carries: an `agent-thread` row's on its
 * originThread, an `agent-wake` row's on its originWake. null for a row
 * written before them or with none.
 */
export function userMessageFacts(meta: UserMessageMeta): MessageFacts | null {
  const origin = userMessageOrigin(meta);
  const attribution = objectRecord(origin === 'agent-thread' ? meta.originThread : origin === 'agent-wake' ? meta.originWake : null);
  const record = objectRecord(attribution?.facts);
  if (!record) return null;
  const spans = Array.isArray(record.body) ? record.body : [];
  const body: MessageSpan[] = [];
  for (const value of spans) {
    const span = objectRecord(value);
    const start = span?.start;
    const end = span?.end;
    if (!Number.isInteger(start) || !Number.isInteger(end)) return null;
    body.push({ start: start as number, end: end as number });
  }
  const threads: MessageFactsThread[] = [];
  for (const value of Array.isArray(record.threads) ? record.threads : []) {
    const thread = objectRecord(value);
    if (!thread) continue;
    threads.push({
      threadId: stringField(thread.threadId),
      title: textField(thread.title),
      computerId: stringField(thread.computerId),
      computerName: textField(thread.computerName),
      age: stringField(thread.age),
      tokens: stringList(thread.tokens),
      role: typeof thread.role === 'string' ? thread.role : null,
    });
  }
  return {
    outcome: stringField(record.outcome),
    body,
    headline: textField(record.headline),
    waiting: record.waiting === true,
    reachable: record.reachable === true,
    userMessage: textField(record.userMessage),
    routesPrompts: record.routesPrompts === true,
    role: typeof record.role === 'string' ? record.role : null,
    tokens: stringList(record.tokens),
    age: stringField(record.age),
    stoppedBy: textField(record.stoppedBy),
    clipped: record.clipped === true,
    madeIn: stringField(record.madeIn),
    now: stringField(record.now),
    threads,
  };
}

const utf8 = new TextEncoder();
const utf8Text = new TextDecoder();

/**
 * The text spans cover in text, joined as they are: the TypeScript twin of
 * threadtools.SpanText. A span past the text's end is cut at it; a span
 * that cuts a character reads it as the replacement character.
 */
export function spanText(text: string, spans: readonly MessageSpan[]): string {
  if (spans.length === 0) return '';
  const bytes = utf8.encode(text);
  // Offsets are characters too when every character is one byte.
  const ascii = bytes.length === text.length;
  let out = '';
  for (const span of spans) {
    const start = Math.min(Math.max(span.start, 0), bytes.length);
    const end = Math.min(Math.max(span.end, 0), bytes.length);
    if (start < end) out += ascii ? text.slice(start, end) : utf8Text.decode(bytes.subarray(start, end));
  }
  return out;
}

/** One member of a joined row: its text and its own attribution. */
export interface JoinedMember {
  text: string;
  meta: UserMessageMeta;
}

const JOIN_SEPARATOR_BYTES = new TextEncoder().encode('\n\n---\n\n');

/**
 * Cut a joined row's text into the members its `joinedParts` record, each
 * with its own attribution: the TypeScript twin of usermessage
 * SplitJoinedParts. A member's text is free text and can hold the separator,
 * so only the recorded UTF-8 lengths cut it. null when the row records no
 * parts or they do not describe its text exactly.
 */
export function splitJoinedParts(text: string, meta: UserMessageMeta): JoinedMember[] | null {
  if (!Array.isArray(meta.joinedParts) || meta.joinedParts.length === 0) return null;
  const bytes = new TextEncoder().encode(text);
  const decoder = new TextDecoder('utf-8', { fatal: true });
  const members: JoinedMember[] = [];
  let at = 0;
  for (const [index, value] of (meta.joinedParts as unknown[]).entries()) {
    const part = objectRecord(value);
    if (!part) return null;
    if (index > 0) {
      if (!JOIN_SEPARATOR_BYTES.every((byte, offset) => bytes[at + offset] === byte)) return null;
      at += JOIN_SEPARATOR_BYTES.length;
    }
    const length = part.bytes;
    if (typeof length !== 'number' || !Number.isInteger(length) || length < 0 || length > bytes.length - at) return null;
    let member: string;
    try {
      member = decoder.decode(bytes.subarray(at, at + length));
    } catch {
      // A length that cuts a character is not this text's.
      return null;
    }
    members.push({
      text: member,
      meta: { sendId: part.sendId, origin: part.origin, originThread: part.originThread, originWake: part.originWake },
    });
    at += length;
  }
  return at === bytes.length ? members : null;
}

/**
 * What wrote an `agent-wake` row: `loop` or `watch` with its label, `send`
 * for a script's outside message, or `question` or `approval` for a worker's
 * prompt put to the standing agent that spawned it, labelled with the
 * worker's title. An `assistant` or `handoff` wake is the app's opening
 * block of an assistant's thread, labelled with the assistant's name. A row
 * without a known kind is not attribution and is refused.
 */
export interface UserMessageOriginWake {
  kind: 'loop' | 'watch' | 'send' | 'question' | 'approval' | 'completion' | 'workflow' | 'assistant' | 'handoff';
  /** The loop, watch or worker; a `completion`'s job; a `workflow`'s id. */
  label: string;
  /** The worker of a `question` or `approval` wake; empty otherwise. */
  threadId: string;
  /** The computer a `completion`'s job ran on; empty otherwise. */
  computer: string;
}

const WAKE_KINDS: ReadonlySet<string> = new Set(['loop', 'watch', 'send', 'question', 'approval', 'completion', 'workflow', 'assistant', 'handoff']);
const WAKE_KIND_LABELS: Record<Exclude<UserMessageOriginWake['kind'], 'send'>, string> = {
  loop: 'Loop',
  watch: 'Watch',
  question: 'Question',
  approval: 'Approval',
  completion: 'Remote job',
  workflow: 'Workflow',
  assistant: 'Opening',
  handoff: 'New thread',
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
    computer: stringField(record.computer),
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

/** Words as written, spaces kept. */
function textField(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function stringList(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((entry): entry is string => typeof entry === 'string' && entry !== '') : [];
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

// The predicates below run over every user row of a pane on each queue
// and reveal pass. Their answers are remembered per row object for as long
// as the row carries the same meta string, so a pass parses only the rows
// that changed.
interface RowFlags { meta: string | undefined; pendingFlush: boolean; wireOnly: boolean; agentMessage: boolean }
const rowFlags = new WeakMap<Item, RowFlags>();
function flagsOf(item: Item): RowFlags {
  let flags = rowFlags.get(item);
  if (flags === undefined || flags.meta !== item.meta) {
    const meta = parseUserMessageMeta(item.meta);
    flags = {
      meta: item.meta,
      pendingFlush: meta.pendingFlush === true,
      wireOnly: meta.wire_only === true,
      agentMessage: isAgentMessageMeta(meta),
    };
    rowFlags.set(item, flags);
  }
  return flags;
}

/** A quiet stored copy, before provider consumption or interrupt promotion. */
export function isPendingFlushRow(item: Item): boolean {
  return item.kind === 'user_text' && flagsOf(item).pendingFlush;
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
 * the composer's recall, and any future "which user messages are the
 * person's" question all route here so they cannot drift.
 */
export function isReaderAuthoredUserText(item: Item): boolean {
  if (item.kind !== 'user_text') return false;
  if ((item.parentId ?? '') !== '') return false;
  const flags = flagsOf(item);
  return !flags.wireOnly && !flags.agentMessage;
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

  const source: AttachmentPreviewSource = {
    id,
    threadId,
    filename: stringField(record.filename) || id,
    mimeType,
    size,
    kind,
  };
  const shell = kind === 'file' ? shellFromMeta(record.shell) : null;
  if (shell) source.shell = shell;
  return source;
}

function shellFromMeta(value: unknown): ShellOutputMeta | null {
  const record = objectRecord(value);
  if (!record) return null;
  const command = stringField(record.command);
  if (!command) return null;
  const exitCode = typeof record.exitCode === 'number' && Number.isFinite(record.exitCode) ? record.exitCode : 0;
  const durationMs = typeof record.durationMs === 'number' && Number.isFinite(record.durationMs) ? record.durationMs : 0;
  const shell: ShellOutputMeta = { command, exitCode, durationMs };
  if (typeof record.commandLength === 'number' && record.commandLength > command.length) {
    shell.commandLength = record.commandLength;
  }
  if (record.truncated === true) shell.truncated = true;
  if (record.timedOut === true) shell.timedOut = true;
  if (record.more === true) shell.more = true;
  if (typeof record.preview === 'string' && record.preview !== '') shell.preview = record.preview;
  return shell;
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
