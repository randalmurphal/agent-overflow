/**
 * A message another agent wrote into this thread, read for a person.
 *
 * On the provider wire these are user-role turns and the store keeps them
 * as `user_text` rows with their origin meta (isAgentMessageMeta). The
 * timeline renders them as an agent-message row instead of the user bubble
 * (`chat/AgentMessageRow.svelte`): who sent it, what kind of message it is,
 * its first line, and, expanded, its body with the routing the agent reads
 * shown as facts.
 *
 * Every message the app writes for an agent stores what it says beside its
 * text (userMessageFacts, threadtools.MessageFacts), made from the same
 * value as the text, and the row renders from those fields. Only a row main
 * wrote before them is read from its text (the legacy reader below): a
 * request with main's footer, or main's answer for one thread. Both ends are
 * pinned by internal/threadtools/testdata/agent_messages.json, which the Go
 * templates write and agentMessage.test.ts reads.
 */

import type { Item } from '../types/models';
import {
  isAgentMessageMeta,
  parseUserMessageMeta,
  spanText,
  splitJoinedParts,
  userMessageFacts,
  userMessageOrigin,
  userMessageOriginThread,
  userMessageOriginWake,
  type MessageFacts,
  type UserMessageMeta,
  type UserMessageOriginThread,
} from './userMessageMeta';
import { readableWakeText } from './wakeText';

/** The routing facts of a request's footer. */
export interface RequestFooterFacts {
  title: string;
  threadId: string;
  /** The sender's computer when it is another one; '' on this computer. */
  computer: string;
  token: string;
  /** The sender is waiting for the answer (it waited or asked to be told). */
  waiting: boolean;
  /** This computer can reach the sender's, so its thread can be read and answered. */
  reachable: boolean;
  /** The sender thread's latest message from the person, quoted; '' for none. */
  userMessage: string;
  /** The sender's role line when it is a standing agent ('' for none); null when it is not one. */
  agentRole: string | null;
  /** The sender spawned this thread and answers its questions and approvals. */
  routesPrompts: boolean;
}

export type WakeKind =
  | 'reply'
  | 'finished'
  | 'errored'
  | 'cancelled'
  | 'interrupted'
  | 'expired'
  | 'reminder'
  | 'settled'
  | 'worker-stopped';

/** One thread a digest wake lists. */
export interface WakeThreadFacts {
  title: string;
  threadId: string;
  /** The thread's computer as the text names it; '' for the caller's own. */
  computer: string;
  /** That computer's id, which the row opens the thread through; '' for the caller's own. */
  computerId: string;
  age: string;
  /** The requests of this thread the message answers, oldest first. */
  tokens: string[];
  agentRole: string | null;
}

/**
 * An answer delivered back to the thread that asked. One message can answer
 * several requests: one thread's turn that took in several of them names
 * every token, and a digest of several threads that gave the same outcome
 * lists each thread in `threads` and leaves the single-thread fields empty.
 */
export interface WakeFacts {
  kind: WakeKind;
  title: string;
  threadId: string;
  computer: string;
  /** How long ago the answer was written, when the status line says. */
  age: string;
  /** The first of `tokens`: the one a clipped answer points thread_status at. */
  token: string;
  /** Every request the message answers, oldest first. */
  tokens: string[];
  agentRole: string | null;
  /**
   * What stopped an `interrupted` request: a computer's restart, the person,
   * its session ending, a move or another interrupt.
   */
  stoppedBy: string;
  /** The answer was cut to the wake's preview; thread_status has the rest. */
  clipped: boolean;
  /** A digest's threads, two or more; empty for a wake from one thread. */
  threads: WakeThreadFacts[];
  /**
   * The assistant's past thread the request was made in and its current
   * thread, when the wake reached the current one after New Thread, Same
   * Assistant; '' otherwise.
   */
  madeIn: string;
  now: string;
}

/** What one message says. */
export interface AgentMessagePart {
  /** What the sender wrote, or the answer a wake carries. */
  body: string;
  footer: RequestFooterFacts | null;
  wake: WakeFacts | null;
}

/** One message of a row with who sent it. A joined row holds several. */
export interface AgentMessageRowPart extends AgentMessagePart {
  source: AgentMessageSource;
}

export type AgentMessageKind =
  | 'spawn'
  | 'request'
  | 'question'
  | WakeKind
  | 'loop'
  | 'watch'
  | 'script'
  | 'worker-question'
  | 'worker-approval'
  | 'job'
  | 'workflow'
  | 'opening'
  | 'handoff'
  | 'message'
  | 'task'
  | 'final-answer';

/** Who sent a message and what kind of message it is. */
export interface AgentMessageSource {
  kind: AgentMessageKind;
  /** Short gutter label, like a tool row's tool name. */
  kindLabel: string;
  /** The kind in words, for the expanded row and assistive technology. */
  kindTitle: string;
  /** Who sent it, as the row names them. */
  sender: string;
  /**
   * The sender's thread, when there is one: the row opens it from the
   * sender and names its computer when that is another one. It comes from
   * the message's attribution, never from a name in its text.
   */
  senderThread: UserMessageOriginThread | null;
  /** The sender's role line when it is a standing agent. */
  senderRole: string;
}

/**
 * The row's whole reading. The row is headed by its first message's source;
 * each part keeps its own, since a joined row's members can come from
 * different senders and be of different kinds.
 */
export interface AgentMessage extends AgentMessageSource {
  firstLine: string;
  parts: AgentMessageRowPart[];
}

function sourced(source: AgentMessageSource, firstLine: string, parts: AgentMessagePart[]): AgentMessage {
  return { ...source, firstLine, parts: parts.map((part) => ({ ...part, source })) };
}

/**
 * A thread a digest lists, as its row can open it: on the caller's own
 * computer or, through its id, on the one it is on. The row's origin chip
 * decides whether this client can reach it.
 */
export function digestThreadOrigin(thread: WakeThreadFacts): UserMessageOriginThread | null {
  if (thread.threadId === '') return null;
  return {
    computerId: thread.computerId, computerName: thread.computer, threadId: thread.threadId, title: thread.title,
    token: thread.tokens[0] ?? '', kind: 'wake',
  };
}

// Gutter labels fit the tool gutter at its narrowest (compact), five
// characters; the title says the kind in full.
const KIND_LABELS: Record<AgentMessageKind, { label: string; title: string }> = {
  reply: { label: 'Reply', title: 'Reply' },
  finished: { label: 'Final', title: 'Final message, no reply' },
  errored: { label: 'Error', title: 'Request errored' },
  cancelled: { label: 'Stop', title: 'Request cancelled' },
  interrupted: { label: 'Stop', title: 'Request interrupted' },
  expired: { label: 'Stale', title: 'Request expired' },
  reminder: { label: 'Alarm', title: 'Reminder' },
  settled: { label: 'Wake', title: 'Request settled' },
  'worker-stopped': { label: 'Stop', title: 'Worker stopped by the user' },
  spawn: { label: 'Spawn', title: 'Spawn prompt' },
  request: { label: 'Send', title: 'Request' },
  question: { label: 'Ask', title: 'Question' },
  loop: { label: 'Loop', title: 'Loop wake' },
  watch: { label: 'Watch', title: 'Watch wake' },
  script: { label: 'Shell', title: 'Message from a script' },
  'worker-question': { label: 'Ask', title: "Worker's question" },
  'worker-approval': { label: 'Allow', title: "Worker's approval request" },
  job: { label: 'Job', title: 'Remote job finished' },
  workflow: { label: 'Run', title: 'Workflow wake' },
  opening: { label: 'Start', title: "Assistant's opening message" },
  handoff: { label: 'Moved', title: 'New thread, same assistant' },
  message: { label: 'Msg', title: 'Message' },
  task: { label: 'Task', title: 'Task' },
  'final-answer': { label: 'Final', title: 'Final answer' },
};

function source(kind: AgentMessageKind, sender: string, senderThread: UserMessageOriginThread | null, senderRole = ''): AgentMessageSource {
  const { label, title } = KIND_LABELS[kind];
  return { kind, kindLabel: label, kindTitle: title, sender, senderThread, senderRole };
}

/** The first non-empty line of `text`, whitespace collapsed. */
export function firstLineOf(text: string): string {
  for (const line of text.split('\n')) {
    const trimmed = line.replace(/^>\s?/, '').trim();
    if (trimmed !== '' && trimmed !== '---') return trimmed.replace(/\s+/g, ' ');
  }
  return '';
}

const REQUEST_KINDS: Record<string, AgentMessageKind> = { spawn: 'spawn', send: 'request', ask: 'question' };
const WAKE_KINDS: ReadonlySet<string> = new Set<WakeKind>([
  'reply', 'finished', 'errored', 'cancelled', 'interrupted', 'expired', 'reminder', 'settled', 'worker-stopped',
]);

function emptyWake(kind: WakeKind): WakeFacts {
  return {
    kind, title: '', threadId: '', computer: '', age: '', token: '', tokens: [],
    agentRole: null, stoppedBy: '', clipped: false, threads: [], madeIn: '', now: '',
  };
}

/** An `agent-thread` row: a request, or the answer coming back. */
function fromThread(text: string, meta: UserMessageMeta): AgentMessage {
  const facts = userMessageFacts(meta);
  if (!facts) return fromLegacyThread(text, meta);
  // The thread it opens, which a reminder's attribution does not name; the
  // fields are read either way.
  const origin = userMessageOriginThread(meta);
  const raw = meta.originThread as Record<string, unknown>;
  const field = (name: string) => (typeof raw[name] === 'string' ? (raw[name] as string).trim() : '');
  const body = spanText(text, facts.body);
  const role = facts.role ?? '';
  if (facts.outcome === '') {
    const kind = REQUEST_KINDS[field('kind')] ?? 'request';
    const footer: RequestFooterFacts = {
      title: field('title'), threadId: field('threadId'), computer: field('computerName'), token: field('token'),
      waiting: facts.waiting, reachable: facts.reachable, userMessage: facts.userMessage,
      agentRole: facts.role, routesPrompts: facts.routesPrompts,
    };
    return sourced(source(kind, field('title') || 'another thread', origin, role), firstLineOf(body), [{ body, footer, wake: null }]);
  }
  const kind: WakeKind = WAKE_KINDS.has(facts.outcome) ? facts.outcome as WakeKind : 'settled';
  const wake = wakeFrom(kind, facts);
  if (facts.threads.length > 1) {
    // A digest speaks for several threads, which it lists; no one of them
    // is its sender.
    const names = facts.threads.map((thread) => thread.title || thread.threadId).join(', ');
    return sourced(source(kind, `${facts.threads.length} threads`, null), firstLineOf(body) || names, [{ body, footer: null, wake }]);
  }
  if (kind === 'reminder') {
    // The thread's own, set by its agent.
    return sourced(source(kind, 'This thread', null), firstLineOf(body), [{ body, footer: null, wake }]);
  }
  Object.assign(wake, { title: field('title'), threadId: field('threadId'), computer: field('computerName'), age: facts.age, agentRole: facts.role });
  return sourced(source(kind, field('title') || 'another thread', origin, role), firstLineOf(body), [{ body, footer: null, wake }]);
}

function wakeFrom(kind: WakeKind, facts: MessageFacts): WakeFacts {
  return {
    ...emptyWake(kind),
    token: facts.tokens[0] ?? '',
    tokens: facts.tokens,
    stoppedBy: facts.stoppedBy,
    clipped: facts.clipped,
    madeIn: facts.madeIn,
    now: facts.now,
    threads: facts.threads.length > 1
      ? facts.threads.map((thread) => ({
          title: thread.title, threadId: thread.threadId, computer: thread.computerName, computerId: thread.computerId,
          age: thread.age, tokens: thread.tokens, agentRole: thread.role,
        }))
      : [],
  };
}

const WAKE_ORIGIN_KINDS: Record<string, AgentMessageKind> = {
  loop: 'loop',
  watch: 'watch',
  send: 'script',
  question: 'worker-question',
  approval: 'worker-approval',
  completion: 'job',
  workflow: 'workflow',
  assistant: 'opening',
  handoff: 'handoff',
};

/**
 * An `agent-wake` row: a report the app wrote, from its facts. main wrote
 * none, so one without facts is a row whose attribution did not decode,
 * shown whole.
 */
function fromWake(text: string, meta: UserMessageMeta): AgentMessage {
  const decoded = userMessageOriginWake(meta);
  const wake = decoded ?? { kind: 'send' as const, label: '', threadId: '', computer: '' };
  const kind = WAKE_ORIGIN_KINDS[wake.kind];
  let sender = wake.label || KIND_LABELS[kind].title;
  if (wake.kind === 'send') sender = decoded ? 'A script' : 'A wake';
  if (wake.kind === 'completion') sender = [wake.label, wake.computer].filter(Boolean).join(' on ') || 'A remote job';
  // An assistant's opening and handoff blocks are the app's own prose
  // (docs/specs/standing-agents.md, "Standing agent").
  if (kind === 'opening' || kind === 'handoff') sender = 'Agent Overflow';
  const senderThread = wake.threadId !== ''
    ? { computerId: '', computerName: '', threadId: wake.threadId, title: wake.label, token: '', kind: '' }
    : null;
  const facts = decoded ? userMessageFacts(meta) : null;
  let body = text;
  let firstLine = '';
  if (facts) {
    body = spanText(text, facts.body);
    // A report's data is quoted for the agent and decoded for the person;
    // a job's output and the app's prose are shown as written.
    if (kind !== 'job' && kind !== 'opening' && kind !== 'handoff') body = readableWakeText(body);
    firstLine = facts.headline;
  }
  return sourced(source(kind, sender, senderThread), firstLine || firstLineOf(body), [{ body, footer: null, wake: null }]);
}

// The legacy reader: rows main wrote, with no facts. main's footer had no
// standing-agent or routing line, and main attributed only a wake for one
// thread to it; a wake for several was the app's own row.

const FOOTER_MARKER = 'Agent request from thread ';
const WAITING_PREFIX = 'It is waiting for your answer. When you are done, call thread_reply with token ';
const NOT_WAITING_PREFIX = 'It is not waiting for an answer. If it should hear something back, call thread_reply with token ';
const REACHABLE = " To read the sender's thread, use thread_show with its id.";
const USER_MESSAGE_PREFIX = "The user's latest message in that thread: ";
const PREVIEW_POINTER = /\n\[preview; thread_status \S+ for the whole answer\]$/;

/**
 * Read a Go `%q` literal starting at `start` (the opening quote). Returns the
 * decoded value and the index past the closing quote, or null when there is
 * no well-formed literal there.
 */
export function readGoQuoted(text: string, start: number): { value: string; end: number } | null {
  if (text[start] !== '"') return null;
  let value = '';
  for (let i = start + 1; i < text.length; i++) {
    const char = text[i];
    if (char === '"') return { value, end: i + 1 };
    if (char === '\n') return null;
    if (char !== '\\') {
      value += char;
      continue;
    }
    const next = text[++i];
    switch (next) {
      case 'a': value += '\x07'; break;
      case 'b': value += '\b'; break;
      case 'f': value += '\f'; break;
      case 'n': value += '\n'; break;
      case 'r': value += '\r'; break;
      case 't': value += '\t'; break;
      case 'v': value += '\v'; break;
      case '\\': value += '\\'; break;
      case '"': value += '"'; break;
      case 'x':
      case 'u':
      case 'U': {
        const width = next === 'x' ? 2 : next === 'u' ? 4 : 8;
        const hex = text.slice(i + 1, i + 1 + width);
        if (!/^[0-9a-fA-F]+$/.test(hex) || hex.length !== width) return null;
        const code = Number.parseInt(hex, 16);
        // A `\x` escape is a byte Go could not print as UTF-8; it reads as
        // the replacement character, as a browser decodes it.
        value += next === 'x' || code > 0x10ffff ? '�' : String.fromCodePoint(code);
        i += width;
        break;
      }
      default:
        return null;
    }
  }
  return null;
}

/** `"title" (id[, on computer][, answered age ago])` at the start of `text`. */
function readThreadNaming(text: string): { title: string; threadId: string; computer: string; age: string; rest: string } | null {
  const quoted = readGoQuoted(text, 0);
  if (!quoted || !text.startsWith(' (', quoted.end)) return null;
  const close = text.indexOf(')', quoted.end + 2);
  if (close < 0) return null;
  let inner = text.slice(quoted.end + 2, close);
  let age = '';
  const answered = /, answered (\S+) ago$/.exec(inner);
  if (answered) {
    age = answered[1];
    inner = inner.slice(0, answered.index);
  }
  let computer = '';
  const on = inner.indexOf(', on ');
  if (on >= 0) {
    computer = inner.slice(on + 5);
    inner = inner.slice(0, on);
  }
  return { title: quoted.value, threadId: inner, computer, age, rest: text.slice(close + 1) };
}

/** main's footer at `lines[at]` (the `---` line): its facts and the index past it, or null. */
function readLegacyFooter(lines: string[], at: number): { facts: RequestFooterFacts; next: number } | null {
  if (lines[at] !== '---' || !lines[at + 1]?.startsWith(FOOTER_MARKER)) return null;
  const naming = readThreadNaming(lines[at + 1].slice(FOOTER_MARKER.length));
  if (!naming || naming.rest !== '.' || naming.age !== '') return null;
  const answer = lines[at + 2] ?? '';
  const prefix = answer.startsWith(WAITING_PREFIX) ? WAITING_PREFIX : answer.startsWith(NOT_WAITING_PREFIX) ? NOT_WAITING_PREFIX : '';
  const token = prefix === '' ? null : /^(\S+), once[.:]/.exec(answer.slice(prefix.length));
  if (!token) return null;
  const facts: RequestFooterFacts = {
    title: naming.title, threadId: naming.threadId, computer: naming.computer, token: token[1],
    waiting: prefix === WAITING_PREFIX, reachable: answer.endsWith(REACHABLE), userMessage: '',
    agentRole: null, routesPrompts: false,
  };
  let next = at + 3;
  if (lines[next]?.startsWith(USER_MESSAGE_PREFIX)) {
    const quoted = readGoQuoted(lines[next], USER_MESSAGE_PREFIX.length);
    if (quoted && quoted.end === lines[next].length) {
      facts.userMessage = quoted.value;
      next++;
    }
  }
  return { facts, next };
}

/**
 * Split main's request text into its messages: each body with the footer
 * after it. A footer is the `---` line opening with the marker;
 * QuoteFooterMimics keeps a body from holding one. main recorded no joined
 * members, so requests a provider joined are told apart by their footers.
 * Text with no footer returns null.
 */
export function parseLegacyRequestText(text: string): AgentMessagePart[] | null {
  const lines = text.split('\n');
  const parts: AgentMessagePart[] = [];
  let bodyStart = 0;
  for (let i = 0; i < lines.length; i++) {
    const footer = lines[i] === '---' ? readLegacyFooter(lines, i) : null;
    if (!footer) continue;
    parts.push({ body: lines.slice(bodyStart, i).join('\n').trim(), footer: footer.facts, wake: null });
    bodyStart = footer.next;
    // A joined message continues after the separator: an empty line, a rule
    // and another empty line.
    if (lines[footer.next] === '' && lines[footer.next + 1] === '---' && lines[footer.next + 2] === '') bodyStart = footer.next + 3;
    i = bodyStart - 1;
  }
  if (parts.length === 0) return null;
  const tail = lines.slice(bodyStart).join('\n').trim();
  if (tail !== '') parts.push({ body: tail, footer: null, wake: null });
  return parts;
}

/** main's status lines after a thread's naming and tokens, with where each puts its answer. */
const LEGACY_TAILS: ReadonlyArray<{ kind: WakeKind; opening: string; body: 'inline' | 'next' | 'none'; stoppedBy?: string }> = [
  { kind: 'finished', opening: ' finished its turn without calling thread_reply. Its final message:', body: 'next' },
  { kind: 'errored', opening: ' errored: ', body: 'inline' },
  { kind: 'errored', opening: ' errored.', body: 'none' },
  { kind: 'cancelled', opening: ' was cancelled.', body: 'none' },
  // The cause the settling code recorded, which main stated as written.
  { kind: 'interrupted', opening: ' was interrupted: ', body: 'inline' },
  { kind: 'interrupted', opening: ' was interrupted.', body: 'none', stoppedBy: 'an interrupt' },
  { kind: 'expired', opening: ' did not answer within a day.', body: 'none' },
  { kind: 'settled', opening: ' settled.', body: 'none' },
];

/** ` [token X]` or ` [tokens X, Y]` at the start of `text`. */
function readTokens(text: string): { tokens: string[]; rest: string } {
  const one = /^ \[token ([^\]\s]+)\]/.exec(text);
  if (one) return { tokens: [one[1]], rest: text.slice(one[0].length) };
  const several = /^ \[tokens ([^\]\s,]+(?:, [^\]\s,]+)+)\]/.exec(text);
  if (several) return { tokens: several[1].split(', '), rest: text.slice(several[0].length) };
  return { tokens: [], rest: text };
}

/** Parse main's wake for one thread: its status line, then its answer. null when the text is not one. */
export function parseLegacyWakeText(text: string): AgentMessagePart | null {
  const newline = text.indexOf('\n');
  const head = newline < 0 ? text : text.slice(0, newline);
  const after = newline < 0 ? '' : text.slice(newline + 1);
  let wake: WakeFacts;
  let body = '';
  if (head.startsWith('Reminder you set')) {
    const { tokens, rest } = readTokens(head.slice('Reminder you set'.length));
    if (rest !== ':') return null;
    wake = { ...emptyWake('reminder'), tokens, token: tokens[0] ?? '' };
    body = after;
  } else {
    const reply = head.startsWith('Reply from ');
    const naming = readThreadNaming(reply ? head.slice('Reply from '.length) : head);
    if (!naming) return null;
    const { tokens, rest } = readTokens(naming.rest);
    const tail = reply ? null : LEGACY_TAILS.find((candidate) => rest.startsWith(candidate.opening));
    if (reply ? rest !== ':' : !tail) return null;
    const inline = tail ? rest.slice(tail.opening.length) : '';
    if (tail?.body === 'inline') body = after === '' ? inline : `${inline}\n${after}`;
    else if (inline !== '' || (tail?.body === 'none' && after !== '')) return null;
    else body = after;
    wake = {
      ...emptyWake(tail?.kind ?? 'reply'),
      title: naming.title, threadId: naming.threadId, computer: naming.computer, age: naming.age,
      tokens, token: tokens[0] ?? '', stoppedBy: tail?.stoppedBy ?? '',
    };
  }
  if (PREVIEW_POINTER.test(body)) {
    wake.clipped = true;
    body = body.replace(PREVIEW_POINTER, '');
  }
  return { body, footer: null, wake };
}

/** A row main wrote, read from its text. */
function fromLegacyThread(text: string, meta: UserMessageMeta): AgentMessage {
  const origin = userMessageOriginThread(meta);
  const sendId = typeof meta.sendId === 'string' ? meta.sendId : '';
  const wake = sendId.startsWith('thread-wake') ? parseLegacyWakeText(text) : null;
  // A row that reads as neither is still the agent's: shown whole.
  const parts = (wake ? [wake] : parseLegacyRequestText(text)) ?? [{ body: text, footer: null, wake: null }];
  const lead = parts[0];
  const kind: AgentMessageKind = lead.wake?.kind ?? 'request';
  if (kind === 'reminder') return sourced(source(kind, 'This thread', null), firstLineOf(lead.body), parts);
  const sender = origin?.title || lead.footer?.title || lead.wake?.title || 'another thread';
  return sourced(source(kind, sender, origin), firstLineOf(lead.body), parts);
}

const CODEX_MESSAGE_KINDS: Record<string, AgentMessageKind> = {
  NEW_TASK: 'task',
  MESSAGE: 'message',
  FINAL_ANSWER: 'final-answer',
};

/** A provider's own agent messages: a Claude peer session's or a Codex collab agent's. */
function fromProvider(text: string, meta: UserMessageMeta & { agent_message?: unknown }): AgentMessage | null {
  const delivery = meta.agent_message;
  if (delivery && typeof delivery === 'object' && !Array.isArray(delivery)) {
    const record = delivery as Record<string, unknown>;
    if (typeof record.sender !== 'string') return null;
    const kind = CODEX_MESSAGE_KINDS[typeof record.messageType === 'string' ? record.messageType : ''] ?? 'message';
    const sender = record.sender === '/root' ? 'Main agent' : record.sender;
    return sourced(source(kind, sender, null), firstLineOf(text), [{ body: text, footer: null, wake: null }]);
  }
  if (userMessageOrigin(meta) !== 'peer-session' || meta.expandComposerCommands === true) return null;
  const name = typeof meta.cross_session_from_name === 'string' ? meta.cross_session_from_name.trim() : '';
  const sender = name ? `${name} (Claude session)` : 'Another Claude session';
  return sourced(source('message', sender, null), firstLineOf(text), [{ body: text, footer: null, wake: null }]);
}

/**
 * Whether a user_text row renders as an agent message: the meta test
 * agentMessageFromItem keys on, without reading the text.
 */
export function isAgentMessageItem(item: Pick<Item, 'kind' | 'meta'>): boolean {
  if (item.kind !== 'user_text') return false;
  const meta = parseUserMessageMeta(item.meta) as UserMessageMeta & { agent_message?: unknown };
  if (isAgentMessageMeta(meta)) return true;
  const delivery = meta.agent_message;
  if (delivery && typeof delivery === 'object' && typeof (delivery as Record<string, unknown>).sender === 'string') return true;
  return userMessageOrigin(meta) === 'peer-session' && meta.expandComposerCommands !== true;
}

/**
 * The agent message a user_text row carries, or null when the row is the
 * person's. Keyed on the row's origin meta.
 */
export function agentMessageFromItem(item: Pick<Item, 'kind' | 'meta' | 'summary'>): AgentMessage | null {
  if (item.kind !== 'user_text') return null;
  const meta = parseUserMessageMeta(item.meta);
  const text = item.summary ?? '';
  if (!isAgentMessageMeta(meta)) return fromProvider(text, meta);
  // A joined row is cut where its recorded members end, never by reading
  // its text, and each member is read with its own attribution, which its
  // parts keep. A row whose members are unknown is read whole.
  const members = splitJoinedParts(text, meta);
  if (!members || members.length < 2) return fromAgent(text, meta);
  const messages = members.map((member) => fromAgent(member.text, member.meta));
  const lead = messages.find((message) => message !== null);
  if (!lead) return fromAgent(text, meta);
  // A member that is not an agent's: its sender is not known.
  const unknown = source('message', 'An unknown sender', null);
  return {
    ...lead,
    parts: messages.flatMap((message, index) =>
      message?.parts ?? [{ body: members[index].text, footer: null, wake: null, source: unknown }]),
  };
}

/** An agent's message read with its attribution; null for a member that is not an agent's. */
function fromAgent(text: string, meta: UserMessageMeta): AgentMessage | null {
  switch (userMessageOrigin(meta)) {
    case 'agent-thread':
      return fromThread(text, meta);
    case 'agent-wake':
      return fromWake(text, meta);
    default:
      return null;
  }
}
