/**
 * A message another agent wrote into this thread, read for a person.
 *
 * On the provider wire these are user-role turns and the store keeps them
 * as `user_text` rows with their origin meta (isAgentMessageMeta). The
 * timeline renders them as an agent-message row instead of the user bubble
 * (`chat/AgentMessageRow.svelte`): who sent it, what kind of message it is,
 * its headline, and, expanded, its body with the routing the agent reads
 * shown as facts.
 *
 * The row renders from the facts its served meta carries
 * (threadtools.MessageFacts, userMessageFacts), never from its text, which
 * is written for the agent. Go reads every row a client receives through
 * agentmeta.Describe, rows main wrote before the facts included, and
 * serves each agent message with facts that describe its text. A message
 * whose facts do not describe the text this client holds is shown whole.
 * internal/threadtools/testdata/agent_messages.json, which the Go templates
 * write, pins both ends.
 */

import type { MessageFacts } from '../../../bindings/agent-overflow/internal/threadtools/models';
import type { Item } from '../types/models';
import {
  factsBody,
  isAgentMessageMeta,
  parseUserMessageMeta,
  splitJoinedParts,
  userMessageFacts,
  userMessageOrigin,
  userMessageOriginThread,
  userMessageOriginWake,
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

/**
 * The kinds of message this file names: a thread's request, the answer
 * coming back, a thread's plain message and a provider's own. A report's
 * kind is Go's, named by its category (agentmeta.ReportDescriptor).
 */
export type AgentMessageKind =
  | 'spawn'
  | 'request'
  | 'question'
  | WakeKind
  | 'message'
  | 'task'
  | 'final-answer';

/** Who sent a message and what kind of message it is. */
export interface AgentMessageSource {
  /** An AgentMessageKind, or a report's category. */
  kind: string;
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
  message: { label: 'Msg', title: 'Message' },
  task: { label: 'Task', title: 'Task' },
  'final-answer': { label: 'Final', title: 'Final answer' },
};

function source(kind: AgentMessageKind, sender: string, senderThread: UserMessageOriginThread | null, senderRole = ''): AgentMessageSource {
  const { label, title } = KIND_LABELS[kind];
  return { kind, kindLabel: label, kindTitle: title, sender, senderThread, senderRole };
}

/**
 * White space as Go's unicode.IsSpace reads it, which FirstLine uses:
 * U+0085 is, and U+FEFF is not, the reverse of JavaScript's `\s`.
 */
const SPACE_RUN = /[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+/g;
const EDGE_SPACE = /^[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+|[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$/g;

/**
 * The first non-empty line of `text`, whitespace collapsed: the headline of
 * a message whose facts do not describe its text, and of a provider's own
 * message, which carries none. It is agentmeta.FirstLine, whose reading
 * the store's thread find indexes.
 */
function firstLineOf(text: string): string {
  for (const line of text.split('\n')) {
    const trimmed = line.replace(/^> ?/, '').replace(EDGE_SPACE, '');
    if (trimmed !== '' && trimmed !== '---') return trimmed.replace(SPACE_RUN, ' ');
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

/**
 * An `agent-thread` row: a request, the answer coming back, or a plain
 * message from the thread. A row whose facts do not describe its text is
 * shown whole: a request as the kind its attribution records, anything
 * else as a message.
 */
function fromThread(text: string, meta: UserMessageMeta): AgentMessage {
  const facts = userMessageFacts(text, meta);
  // The thread it opens, which a reminder's attribution does not name; the
  // fields are read either way.
  const origin = userMessageOriginThread(meta);
  const raw = (meta.originThread ?? {}) as Record<string, unknown>;
  const field = (name: string) => (typeof raw[name] === 'string' ? (raw[name] as string).trim() : '');
  const sender = field('title') || 'another thread';
  if (!facts || facts.form === 'whole') {
    const kind = REQUEST_KINDS[field('kind')] ?? (field('kind') === '' ? 'request' : 'message');
    return sourced(source(kind, sender, origin), facts?.headline ?? firstLineOf(text), [{ body: text, footer: null, wake: null }]);
  }
  const body = factsBody(text, facts);
  const headline = facts.headline ?? '';
  const role = facts.role ?? null;
  if (facts.form === 'message') {
    return sourced(source('message', sender, origin, role ?? ''), headline, [{ body, footer: null, wake: null }]);
  }
  if (facts.form === 'request') {
    const footer: RequestFooterFacts = {
      title: field('title'), threadId: field('threadId'), computer: field('computerName'), token: field('token'),
      waiting: facts.waiting === true, reachable: facts.reachable === true, userMessage: facts.userMessage ?? '',
      agentRole: role, routesPrompts: facts.routesPrompts === true,
    };
    return sourced(source(REQUEST_KINDS[field('kind')] ?? 'request', sender, origin, role ?? ''), headline, [{ body, footer, wake: null }]);
  }
  const outcome = facts.outcome ?? '';
  const kind: WakeKind = WAKE_KINDS.has(outcome) ? outcome as WakeKind : 'settled';
  const wake = wakeFrom(kind, facts);
  if (wake.threads.length > 1) {
    // A digest speaks for several threads, which it lists; no one of them
    // is its sender.
    return sourced(source(kind, `${wake.threads.length} threads`, null), headline, [{ body, footer: null, wake }]);
  }
  if (kind === 'reminder') {
    // The thread's own, set by its agent.
    return sourced(source(kind, 'This thread', null), headline, [{ body, footer: null, wake }]);
  }
  Object.assign(wake, { title: field('title'), threadId: field('threadId'), computer: field('computerName'), age: facts.age ?? '', agentRole: role });
  return sourced(source(kind, sender, origin, role ?? ''), headline, [{ body, footer: null, wake }]);
}

function wakeFrom(kind: WakeKind, facts: MessageFacts): WakeFacts {
  const tokens = facts.tokens ?? [];
  const threads = facts.threads ?? [];
  return {
    ...emptyWake(kind),
    token: tokens[0] ?? '',
    tokens,
    stoppedBy: facts.stoppedBy ?? '',
    clipped: facts.clipped === true,
    madeIn: facts.madeIn ?? '',
    now: facts.now ?? '',
    threads: threads.length > 1
      ? threads.map((thread) => ({
          title: thread.title ?? '', threadId: thread.threadId, computer: thread.computerName ?? '', computerId: thread.computerId ?? '',
          age: thread.age ?? '', tokens: thread.tokens ?? [], agentRole: thread.role ?? null,
        }))
      : [],
  };
}

/**
 * A report whose attribution this client cannot read: of a kind the
 * computer serving it does not know, or whose attribution did not decode.
 */
const UNREADABLE_REPORT: AgentMessageSource = {
  kind: 'script', kindLabel: 'Shell', kindTitle: 'Message from a script', sender: 'A wake', senderThread: null, senderRole: '',
};

/**
 * An `agent-wake` row: a report the app wrote, read from its facts and
 * named as Go serves its kind (agentmeta.ReportDescriptor). One whose
 * facts do not describe its text, or whose attribution this client cannot
 * read, is shown whole.
 */
function fromWake(text: string, meta: UserMessageMeta): AgentMessage {
  const wake = userMessageOriginWake(meta);
  if (!wake) return sourced(UNREADABLE_REPORT, firstLineOf(text), [{ body: text, footer: null, wake: null }]);
  const { report } = wake;
  // A worker's prompt opens the worker, on the paired computer that holds it.
  const senderThread = report.opens
    ? { computerId: wake.computerId, computerName: wake.computerId ? wake.computer : '', threadId: report.opens, title: wake.label, token: '', kind: '' }
    : null;
  const reporter: AgentMessageSource = {
    kind: report.category, kindLabel: report.label, kindTitle: report.title, sender: report.sender, senderThread, senderRole: '',
  };
  const facts = userMessageFacts(text, meta);
  if (!facts || facts.form !== 'report') {
    return sourced(reporter, facts?.headline ?? firstLineOf(text), [{ body: text, footer: null, wake: null }]);
  }
  let body = factsBody(text, facts);
  // Data quoted for the agent is decoded for the person.
  if (report.body === 'data') body = readableWakeText(body);
  return sourced(reporter, facts.headline ?? '', [{ body, footer: null, wake: null }]);
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

// isAgentMessageItem's answer per row object, for as long as the row
// carries the same meta string: activity-run grouping asks it of rows on
// every projection pass, and a meta parse each time would be per-pass work
// on rows that did not change.
const agentMessageRows = new WeakMap<Pick<Item, 'kind' | 'meta'>, { meta: string | undefined; agent: boolean }>();

/**
 * Whether a user_text row renders as an agent message: the meta test
 * agentMessageFromItem keys on, without reading the text. How such a row
 * groups into activity runs depends on the thread's `ActivityRunRule`
 * (`isActivityRunMemberRow`); the store's twin of this test is
 * `agentMessageRowExpr`.
 */
export function isAgentMessageItem(item: Pick<Item, 'kind' | 'meta'>): boolean {
  if (item.kind !== 'user_text') return false;
  const cached = agentMessageRows.get(item);
  if (cached !== undefined && cached.meta === item.meta) return cached.agent;
  const agent = readsAsAgentMessage(item.meta);
  agentMessageRows.set(item, { meta: item.meta, agent });
  return agent;
}

function readsAsAgentMessage(rawMeta: string | undefined): boolean {
  const meta = parseUserMessageMeta(rawMeta) as UserMessageMeta & { agent_message?: unknown };
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
