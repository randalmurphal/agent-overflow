// What the expanded body of an AO tool row shows of the call's reply: the
// outcome as a line, the facts a person reads, one line per listed thread,
// request or run, a link to what the call touched, and long text folded to
// its first lines. One decoder per server, each reading that server's own
// reply shapes (`internal/threadtools`, `internal/workflowtools`,
// `internal/browser/mcp.go`, `internal/app/app_remote_mcp_result.go`). A
// reply none of them recognises still reads as facts, never as the JSON
// the model read.
//
// Errors arrive as text, not JSON: the thread and workflow servers write
// `[code] message` (`internal/threadmcp/server.go` WriteToolError) and the
// browser server the message alone. `aoToolError` splits the code off so
// the row shows the message with the code as a chip.

import { BROWSER_TOOLS_SERVER } from '../../utils/browserTools';
import { formatDurationMs, formatTimeOfDay, formatCalendarDate } from '../../utils/format';
import {
  FACT_MAX,
  REMOTE_TOOLS_SERVER,
  THREAD_TOOLS_SERVER,
  WORKFLOW_TOOLS_SERVER,
  runName,
  shortId,
  threadName,
  type AoToolFact,
  type AoToolNames,
} from './aoTools';

type Input = Record<string, unknown>;

/** Something the row can open: a thread in a pane, a run in the workflows overlay. */
export interface AoResultLink {
  kind: 'thread' | 'run';
  id: string;
  label: string;
  /** The computer the thread lives on, when the reply named another one. */
  computerId: string;
  computerName: string;
}

/** One listed entity: a request, a search hit, a run. */
export interface AoResultLine {
  key: string;
  title: string;
  /** State, kind, who spoke last: one short line. */
  detail: string;
  /** The answer's or the snippet's first lines, when there is one. */
  text: string;
  link: AoResultLink | null;
}

/** Long text the body folds to its first lines behind "show more". */
export interface AoResultPreview {
  label: string;
  text: string;
}

export interface AoResultView {
  /** Where the call left things: `running · backgrounded`, `3 matches`. */
  outcome: string;
  link: AoResultLink | null;
  facts: AoToolFact[];
  lines: AoResultLine[];
  preview: AoResultPreview | null;
  /** The reply's own guidance, when it still applies. */
  note: string;
}

/** A remote tool reply as its row shows it: the outcome, then the output. */
export interface RemoteResultView {
  /** `succeeded · exit 0 · 12.0s`, `running in the background`, `failed · exit 1`. */
  outcome: string;
  /** The destination's own error, when the reply carries one. */
  error: string;
  /** The oldest output the reply carried, when the tail did not reach the start. */
  head: string;
  /** The newest output the reply carried. */
  output: string;
  /** Guidance the reply carried about output that is not here. */
  hint: string;
}

/** An MCP error result: the message, and the server's code when it gave one. */
export interface AoToolError {
  message: string;
  code: string;
}

/** Lines of a folded preview shown before "show more". */
export const PREVIEW_LINES = 6;
/** Lines listed before the rest fold behind "show more". */
export const LIST_LINES = 8;
/** Characters a line's text keeps. */
const LINE_TEXT_MAX = 320;
/** Characters a preview keeps even when shown in full; the copy button has the rest. */
export const PREVIEW_MAX = 64 * 1024;

const SETTLED_STATES = new Set(['replied', 'finished', 'errored', 'cancelled', 'interrupted', 'expired', 'refused']);

function str(input: Input, key: string): string {
  const value = input[key];
  return typeof value === 'string' ? value.trim() : '';
}

function num(input: Input, key: string): number | null {
  const value = input[key];
  return typeof value === 'number' && Number.isFinite(value) ? value : null;
}

function obj(value: unknown): Input | null {
  return value && typeof value === 'object' && !Array.isArray(value) ? (value as Input) : null;
}

function list(value: unknown): Input[] {
  return Array.isArray(value) ? value.map(obj).filter((v): v is Input => v !== null) : [];
}

function clipText(text: string, max: number): string {
  return text.length <= max ? text : `${text.slice(0, max - 1)}…`;
}

function plural(count: number, noun: string, nouns = `${noun}s`): string {
  return `${count} ${count === 1 ? noun : nouns}`;
}

function bytesText(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

function words(text: string): string {
  return text.replace(/_/g, ' ');
}

function join(...parts: string[]): string {
  return parts.filter(Boolean).join(' · ');
}

/** An RFC 3339 time as the row's other times read: time of day, with the date when it is not today. */
function whenText(value: string): string {
  const at = Date.parse(value);
  if (!Number.isFinite(at)) return value;
  const sameDay = new Date(at).toDateString() === new Date().toDateString();
  return sameDay ? formatTimeOfDay(at) : `${formatCalendarDate(at)} ${formatTimeOfDay(at)}`;
}

/**
 * The reply's first JSON value. A client may append text after it (the
 * browser server's note, Codex's structured content), so a reply that does
 * not parse whole is cut at the end of its first object or array.
 */
export function parseFirstJson(data: string): unknown {
  const text = data.trim();
  if (text === '' || (text[0] !== '{' && text[0] !== '[')) return undefined;
  try {
    return JSON.parse(text);
  } catch {
    // fall through to the first value
  }
  let depth = 0;
  let inString = false;
  let escaped = false;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (inString) {
      if (escaped) escaped = false;
      else if (c === '\\') escaped = true;
      else if (c === '"') inString = false;
      continue;
    }
    if (c === '"') inString = true;
    else if (c === '{' || c === '[') depth++;
    else if (c === '}' || c === ']') {
      depth--;
      if (depth === 0) {
        try {
          return JSON.parse(text.slice(0, i + 1));
        } catch {
          return undefined;
        }
      }
    }
  }
  return undefined;
}

/** Splits `[thread_not_found] No thread matches…` into its code and message. */
export function aoToolError(text: string): AoToolError {
  const trimmed = text.trim();
  const match = /^\[([a-z][a-z0-9_]*)\]\s+([\s\S]*)$/.exec(trimmed);
  return match ? { code: match[1], message: match[2].trim() } : { code: '', message: trimmed };
}

function threadLink(id: string, title: string, computerId: string, computerName: string): AoResultLink | null {
  if (!id) return null;
  return { kind: 'thread', id, label: title || `thread ${shortId(id)}`, computerId, computerName };
}

function runLink(id: string, label: string): AoResultLink | null {
  return id ? { kind: 'run', id, label, computerId: '', computerName: '' } : null;
}

function emptyView(): AoResultView {
  return { outcome: '', link: null, facts: [], lines: [], preview: null, note: '' };
}

function fact(facts: AoToolFact[], label: string, value: string): void {
  if (value) facts.push({ label, value: clipText(value, FACT_MAX) });
}

/** A request state as the row says it. */
function requestState(state: string): string {
  switch (state) {
    case 'blocked':
      return 'blocked on a person';
    case 'replied':
      return 'replied';
    default:
      return words(state);
  }
}

// ---- thread tools ------------------------------------------------------

/** A spawn, send, ask or remind ack (`internal/threadtools/start.go` ackResult). */
function threadAckView(reply: Input, input: Input, names: AoToolNames): AoResultView {
  const view = emptyView();
  const state = str(reply, 'state');
  const outcome = str(reply, 'outcome');
  view.outcome = join(requestState(state), outcome !== 'settled' ? words(outcome) : '');
  const id = str(reply, 'thread_id');
  view.link = threadLink(id, threadName(id, names, str(reply, 'title')), str(reply, 'computer_id'), str(reply, 'computer'));
  // What a spawn overrode: the defaults go unsaid.
  fact(view.facts, 'branch', str(input, 'worktree'));
  if (str(input, 'worktree')) fact(view.facts, 'base', str(input, 'base'));
  fact(view.facts, 'provider', str(input, 'provider'));
  fact(view.facts, 'model', str(input, 'model'));
  fact(view.facts, 'effort', str(input, 'effort'));
  fact(view.facts, 'group', str(input, 'group'));
  const nextFire = str(reply, 'next_fire');
  if (nextFire) fact(view.facts, 'fires', whenText(nextFire));
  fact(view.facts, 'notify', reply.notify === true ? 'armed' : '');
  fact(view.facts, 'request', shortId(str(reply, 'token')));
  const answer = typeof reply.answer === 'string' ? reply.answer : '';
  if (answer) view.preview = { label: `${str(reply, 'answer_kind') || 'answer'}`, text: answer };
  // The note says what to do while the work continues; once the call
  // carried the answer it no longer applies.
  if (outcome !== 'settled' && !SETTLED_STATES.has(state)) view.note = str(reply, 'note');
  return view;
}

function threadReplyView(reply: Input, names: AoToolNames): AoResultView {
  const view = emptyView();
  const accepted = reply.accepted === true;
  view.outcome = join(accepted ? 'accepted' : 'already stored', reply.late === true ? 'late' : '', requestState(str(reply, 'state')));
  const id = str(reply, 'source_thread_id');
  view.link = threadLink(id, threadName(id, names), '', str(reply, 'source_computer'));
  fact(view.facts, 'answered', reply.prompt === true ? "the worker's question" : '');
  fact(view.facts, 'request', shortId(str(reply, 'token')));
  view.note = str(reply, 'note');
  return view;
}

function requestLine(row: Input, names: AoToolNames): AoResultLine {
  const id = str(row, 'thread_id');
  const title = threadName(id, names, str(row, 'title')) || words(str(row, 'kind'));
  const due = num(row, 'due_at');
  const answer = typeof row.answer === 'string' ? row.answer : '';
  return {
    key: str(row, 'token') || id,
    title,
    detail: join(
      words(str(row, 'kind')),
      requestState(str(row, 'state')),
      str(row, 'answer_kind'),
      due ? `fires ${formatTimeOfDay(due)}` : '',
    ),
    text: clipText(answer, LINE_TEXT_MAX),
    link: threadLink(id, title, str(row, 'computer_id'), str(row, 'computer')),
  };
}

function threadStatusView(reply: Input, names: AoToolNames): AoResultView {
  const view = emptyView();
  const requests = list(reply.requests);
  const threads = list(reply.threads);
  const wakes = list(reply.wakes);
  for (const row of requests) view.lines.push(requestLine(row, names));
  for (const row of threads) {
    const id = str(row, 'thread_id');
    const title = threadName(id, names, str(row, 'title'));
    view.lines.push({
      key: id,
      title,
      detail: join(words(str(row, 'state')), row.resting === true ? 'resting' : ''),
      text: '',
      link: threadLink(id, title, str(row, 'computer_id'), str(row, 'computer')),
    });
  }
  for (const row of wakes) {
    const next = str(row, 'next_fire');
    view.lines.push({
      key: `wake:${str(row, 'kind')}:${str(row, 'label')}`,
      title: str(row, 'label') || words(str(row, 'kind')),
      detail: join(str(row, 'kind'), words(str(row, 'state')), next ? `next ${whenText(next)}` : ''),
      text: '',
      link: null,
    });
  }
  const counts = [
    requests.length ? plural(requests.length, 'request') : '',
    threads.length ? plural(threads.length, 'thread') : '',
    wakes.length ? plural(wakes.length, 'loop or watch', 'loops or watches') : '',
  ];
  const woke = str(reply, 'woke_on');
  view.outcome = join(
    counts.filter(Boolean).join(', ') || 'nothing open',
    reply.timed_out === true ? 'wait ran out' : woke ? 'woke on an answer' : '',
    reply.more === true ? 'more' : '',
  );
  view.note = str(reply, 'note');
  return view;
}

function searchLine(row: Input, names: AoToolNames, computerName: string): AoResultLine {
  const id = str(row, 'thread_id');
  const title = threadName(id, names, str(row, 'title'));
  const last = obj(row.last_message);
  const snippet = str(row, 'snippet');
  return {
    key: `${str(row, 'computer_id')}:${id}:${str(row, 'item_id')}`,
    title,
    detail: join(
      words(str(row, 'state')),
      last ? `${str(last, 'role')} spoke last` : '',
      computerName || str(row, 'computer'),
      row.self === true ? 'this thread' : '',
    ),
    text: clipText(snippet || (last ? str(last, 'text') : ''), LINE_TEXT_MAX),
    link: threadLink(id, title, str(row, 'computer_id'), str(row, 'computer')),
  };
}

function threadSearchView(reply: Input, names: AoToolNames): AoResultView {
  const view = emptyView();
  let more = reply.more === true;
  let indexing = reply.indexing === true;
  for (const row of list(reply.rows)) view.lines.push(searchLine(row, names, ''));
  for (const group of list(reply.computers)) {
    more ||= group.more === true;
    indexing ||= group.indexing === true;
    const error = str(group, 'error');
    if (error) {
      fact(view.facts, str(group, 'computer') || 'computer', error);
      continue;
    }
    for (const row of list(group.rows)) view.lines.push(searchLine(row, names, str(group, 'computer')));
  }
  view.outcome = join(
    view.lines.length ? plural(view.lines.length, 'thread') : 'no threads',
    more ? 'more' : '',
    indexing ? 'still indexing' : '',
  );
  view.note = str(reply, 'note');
  return view;
}

function threadShowView(reply: Input, names: AoToolNames): AoResultView {
  const view = emptyView();
  const items = num(reply, 'items') ?? 0;
  const bytes = num(reply, 'bytes') ?? 0;
  view.outcome = join(words(str(reply, 'window')), plural(items, 'item'), bytesText(bytes), reply.done === false ? 'more' : '');
  const id = str(reply, 'thread_id');
  view.link = threadLink(id, threadName(id, names, str(reply, 'title')), str(reply, 'computer_id'), str(reply, 'computer'));
  fact(view.facts, 'state', words(str(reply, 'state')));
  const file = obj(reply.file);
  if (file) fact(view.facts, 'file', join(str(file, 'path'), bytesText(num(file, 'bytes') ?? num(file, 'size') ?? 0)));
  const transcript = typeof reply.transcript === 'string' ? reply.transcript : '';
  if (transcript) view.preview = { label: 'transcript', text: transcript };
  return view;
}

function threadItemView(reply: Input, names: AoToolNames): AoResultView {
  const view = emptyView();
  const size = num(reply, 'size') ?? 0;
  const read = num(reply, 'bytes') ?? 0;
  const matches = list(reply.matches);
  const query = str(reply, 'query');
  view.outcome = query
    ? join(plural(num(reply, 'match_count') ?? matches.length, 'match', 'matches'), reply.more === true ? 'more' : '')
    : join(`${bytesText(read)} of ${bytesText(size)}`, words(str(reply, 'kind')), reply.eof === true ? 'to the end' : '');
  const id = str(reply, 'thread_id');
  view.link = threadLink(id, threadName(id, names), str(reply, 'computer_id'), str(reply, 'computer'));
  fact(view.facts, 'item', str(reply, 'item_id'));
  const first = num(reply, 'first_line');
  if (first) fact(view.facts, 'lines', `${first}-${num(reply, 'last_line') ?? first}`);
  for (const match of matches) {
    const offset = num(match, 'offset') ?? 0;
    view.lines.push({ key: `match:${offset}`, title: `byte ${offset}`, detail: '', text: clipText(str(match, 'context'), LINE_TEXT_MAX), link: null });
  }
  const text = typeof reply.text === 'string' ? reply.text : '';
  if (text) view.preview = { label: 'content', text };
  return view;
}

function threadCancelView(reply: Input, names: AoToolNames): AoResultView {
  const view = emptyView();
  view.outcome = join(words(str(reply, 'effect')), words(str(reply, 'state')));
  const id = str(reply, 'thread_id');
  view.link = threadLink(id, threadName(id, names), str(reply, 'computer_id'), str(reply, 'computer'));
  fact(view.facts, 'label', str(reply, 'label'));
  fact(view.facts, 'request', shortId(str(reply, 'token')));
  view.note = str(reply, 'note');
  return view;
}

function threadUpdateView(reply: Input, names: AoToolNames): AoResultView {
  const view = emptyView();
  const results = list(reply.results).concat(list(reply.computers).flatMap((group) => list(group.results)));
  let updated = 0;
  for (const row of results) {
    const id = str(row, 'thread_id');
    const title = threadName(id, names, str(row, 'title'));
    if (row.updated === true) updated++;
    view.lines.push({
      key: id,
      title,
      detail: row.updated === true ? 'updated' : join('refused', str(row, 'error_code')),
      text: str(row, 'error'),
      link: threadLink(id, title, str(row, 'computer_id'), str(row, 'computer')),
    });
  }
  view.outcome = `${updated} of ${plural(results.length, 'thread')} updated`;
  return view;
}

function threadWakeView(reply: Input): AoResultView {
  const view = emptyView();
  const next = str(reply, 'next_fire');
  view.outcome = join(words(str(reply, 'state')), next ? `next ${whenText(next)}` : '');
  fact(view.facts, 'command', str(reply, 'command'));
  fact(view.facts, 'last run', str(reply, 'last_run') ? whenText(str(reply, 'last_run')) : '');
  fact(view.facts, 'last outcome', str(reply, 'last_outcome'));
  view.note = str(reply, 'note');
  return view;
}

/** Reads a thread tools reply (`internal/threadtools`). */
export function threadResultView(tool: string, data: string, input: Input, names: AoToolNames): AoResultView | null {
  const reply = obj(parseFirstJson(data));
  if (!reply) return null;
  switch (tool) {
    case 'thread_spawn':
    case 'thread_send':
    case 'thread_ask':
    case 'thread_remind':
      return threadAckView(reply, input, names);
    case 'thread_reply':
      return threadReplyView(reply, names);
    case 'thread_status':
      return threadStatusView(reply, names);
    case 'thread_search':
      return threadSearchView(reply, names);
    case 'thread_show':
      return threadShowView(reply, names);
    case 'thread_item':
      return threadItemView(reply, names);
    case 'thread_cancel':
      return threadCancelView(reply, names);
    case 'thread_update':
      return threadUpdateView(reply, names);
    case 'thread_loop':
    case 'thread_watch':
      return threadWakeView(reply);
    case 'thread_group': {
      const view = emptyView();
      const ungrouped = num(reply, 'ungrouped');
      view.outcome = join(`${str(reply, 'action')} ${str(reply, 'group')}`.trim(), ungrouped ? `${plural(ungrouped, 'thread')} ungrouped` : '');
      return view;
    }
    default:
      return factsView(reply);
  }
}

// ---- workflow tools ----------------------------------------------------

/** A run document's state, phase and reason (`WorkflowAgentRunView`). */
function runFacts(view: AoResultView, run: Input): void {
  const phase = str(run, 'currentPhaseId');
  const ordinal = num(run, 'currentPhaseOrdinal');
  const count = num(run, 'phaseCount');
  fact(view.facts, 'phase', phase ? join(phase, ordinal && count ? `${ordinal} of ${count}` : '') : '');
  fact(view.facts, 'reason', str(run, 'reason'));
  const failed = list(run.failedUnits);
  if (failed.length) fact(view.facts, 'failed', plural(failed.length, 'unit'));
  const started = num(run, 'startedAt');
  const ended = num(run, 'endedAt');
  if (started && ended && ended >= started) fact(view.facts, 'ran', formatDurationMs(ended - started));
}

function runTitle(run: Input, names: AoToolNames): string {
  const id = str(run, 'itemId');
  return names.run(id) || join(str(run, 'workflowId'), str(run, 'goal')) || runName(id, names);
}

function workflowRunView(reply: Input, names: AoToolNames): AoResultView {
  const view = emptyView();
  const start = obj(reply.start);
  const run = obj(reply.run) ?? (str(reply, 'itemId') ? reply : null);
  const doc = run ?? start ?? {};
  const outcome = str(reply, 'outcome');
  view.outcome = join(words(str(doc, 'state')), outcome && outcome !== str(doc, 'state') ? words(outcome) : '');
  const id = str(doc, 'itemId') || (start ? str(start, 'itemId') : '');
  view.link = runLink(id, runTitle({ ...(start ?? {}), ...doc, itemId: id }, names));
  if (run) runFacts(view, run);
  if (start) fact(view.facts, 'binding', str(start, 'bindingWarning'));
  if (start?.skipped === true) fact(view.facts, 'start', 'skipped: already running');
  const notify = obj(reply.notify);
  if (notify) fact(view.facts, 'notify', join(notify.armed === true ? 'armed' : 'unavailable', str(notify, 'detail')));
  fact(view.facts, 'read error', str(reply, 'readError'));
  const outputs = obj(reply.outputs);
  if (outputs) {
    for (const [key, value] of Object.entries(outputs)) fact(view.facts, key, scalarText(value));
  }
  const artifacts = Array.isArray(reply.artifacts) ? reply.artifacts.filter((a): a is string => typeof a === 'string') : [];
  if (artifacts.length) fact(view.facts, 'artifacts', artifacts.join('\n'));
  return view;
}

function workflowListView(reply: Input): AoResultView {
  const view = emptyView();
  const workflows = list(reply.workflows);
  for (const row of workflows) {
    const findings = list(row.findings);
    view.lines.push({
      key: `${str(row, 'scope')}:${str(row, 'id')}`,
      title: str(row, 'name') || str(row, 'id'),
      detail: join(str(row, 'id'), str(row, 'scope'), findings.length ? plural(findings.length, 'finding') : ''),
      text: '',
      link: null,
    });
  }
  view.outcome = plural(workflows.length, 'workflow');
  return view;
}

function validationView(reply: Input): AoResultView {
  const view = emptyView();
  const validation = obj(reply.validation) ?? reply;
  const findings = list(validation.findings).concat(list(validation.reports));
  for (const [index, row] of findings.entries()) {
    view.lines.push({
      key: `finding:${index}`,
      title: str(row, 'element') || str(row, 'code'),
      detail: str(row, 'code'),
      text: clipText(str(row, 'message'), LINE_TEXT_MAX),
      link: null,
    });
  }
  const created = Array.isArray(reply.created) ? reply.created.filter((c): c is string => typeof c === 'string') : [];
  if (created.length) fact(view.facts, 'created', created.join('\n'));
  view.outcome = findings.length ? plural(findings.length, 'finding') : 'valid';
  view.note = str(reply, 'note');
  return view;
}

/** Reads a workflow tools reply (`internal/workflowtools`). */
export function workflowResultView(tool: string, data: string, names: AoToolNames): AoResultView | null {
  const parsed = parseFirstJson(data);
  if (Array.isArray(parsed) && tool === 'run_list') {
    const view = emptyView();
    for (const run of list(parsed)) {
      const id = str(run, 'itemId');
      const title = runTitle(run, names);
      view.lines.push({
        key: id,
        title,
        detail: join(words(str(run, 'state')), str(run, 'currentPhaseId')),
        text: '',
        link: runLink(id, title),
      });
    }
    view.outcome = plural(view.lines.length, 'run');
    return view;
  }
  const reply = obj(parsed);
  if (!reply) return null;
  switch (tool) {
    case 'workflow_list':
      return workflowListView(reply);
    case 'workflow_validate':
    case 'workflow_new':
      return validationView(reply);
    case 'workflow_schema':
      return { ...emptyView(), outcome: 'authoring schema', preview: { label: 'schema', text: data } };
    default:
      if (reply.start || reply.run || str(reply, 'itemId')) return workflowRunView(reply, names);
      return factsView(reply);
  }
}

// ---- browser tools -----------------------------------------------------

function pageLine(page: Input): AoResultLine {
  return {
    key: str(page, 'id'),
    title: str(page, 'label') || str(page, 'title') || str(page, 'url') || 'page',
    detail: join(str(page, 'url'), page.selected === true ? 'selected' : '', page.suspended === true ? 'suspended' : ''),
    text: '',
    link: null,
  };
}

/** Reads a browser tools reply (`internal/browser/mcp.go`). */
export function browserResultView(tool: string, data: string): AoResultView | null {
  const parsed = parseFirstJson(data);
  if (tool === 'browser_evaluate' || tool === 'browser_evaluate_readonly') {
    // Any JSON value is a result here, including a bare string or number;
    // the note the server appends after it is guidance.
    const view = emptyView();
    const value = parsed === undefined ? data : JSON.stringify(parsed, null, 2);
    view.outcome = 'returned';
    view.preview = { label: 'value', text: value ?? String(parsed) };
    if (parsed !== undefined) {
      const raw = JSON.stringify(parsed) ?? '';
      const rest = data.trim().slice(raw.length).trim();
      view.note = rest.startsWith('{') ? '' : rest;
    }
    return view;
  }
  if (Array.isArray(parsed)) {
    const view = emptyView();
    for (const page of list(parsed)) view.lines.push(pageLine(page));
    view.outcome = view.lines.length ? plural(view.lines.length, 'page') : 'no pages';
    return view;
  }
  const reply = obj(parsed);
  if (!reply) return null;
  if (typeof reply.url === 'string' && typeof reply.id === 'string') {
    // A page state: every navigation, interaction and snapshot answers with one.
    const view = emptyView();
    view.outcome = str(reply, 'label') || str(reply, 'title') || 'untitled page';
    fact(view.facts, 'url', str(reply, 'url'));
    const elements = list(reply.elements);
    if (elements.length) fact(view.facts, 'elements', String(elements.length));
    const text = typeof reply.text === 'string' ? reply.text : '';
    if (text) view.preview = { label: 'page text', text };
    return view;
  }
  return factsView(reply);
}

// ---- remote tools --------------------------------------------------------

/**
 * Reads a `remote_run`, `remote_status` or `remote_cancel` reply
 * (`internal/app/app_remote_mcp_result.go` remoteMCPResult) for the row's
 * body. Null for any other text.
 */
export function remoteResultView(data: string): RemoteResultView | null {
  const reply = obj(parseFirstJson(data));
  if (!reply) return null;
  const state = str(reply, 'state');
  if (!state || typeof reply.id !== 'string') return null;
  const parts = [state === 'running' && reply.backgrounded === true ? 'running in the background' : state];
  const exit = num(reply, 'exitCode');
  if (state !== 'running' && exit !== null && exit >= 0) parts.push(`exit ${exit}`);
  const started = num(reply, 'startedAt');
  const finished = num(reply, 'finishedAt');
  if (started !== null && finished !== null && finished > 0 && finished >= started) {
    parts.push(formatDurationMs(finished - started));
  }
  return {
    outcome: parts.join(' · '),
    error: str(reply, 'error'),
    head: typeof reply.outputHead === 'string' ? reply.outputHead : '',
    output: typeof reply.output === 'string' ? reply.output : '',
    hint: str(reply, 'outputHint'),
  };
}

/** The other remote replies (listings, logs, fetches): their facts and lines. */
function remoteOtherView(data: string): AoResultView | null {
  const parsed = parseFirstJson(data);
  if (Array.isArray(parsed)) return arrayView(parsed);
  const reply = obj(parsed);
  return reply ? factsView(reply) : null;
}

// ---- any reply ---------------------------------------------------------

function scalarText(value: unknown): string {
  if (typeof value === 'string') return value;
  if (typeof value === 'number' || typeof value === 'boolean') return String(value);
  if (Array.isArray(value)) return plural(value.length, 'item');
  if (value && typeof value === 'object') return plural(Object.keys(value).length, 'field');
  return '';
}

/**
 * A reply no decoder knows: each scalar field as a fact, each list as a
 * count, each array of records as lines. Never the JSON text.
 */
function factsView(reply: Input): AoResultView {
  const view = emptyView();
  for (const [key, value] of Object.entries(reply)) {
    if (key === 'note' && typeof value === 'string') {
      view.note = value;
      continue;
    }
    const records = list(value);
    if (records.length && view.lines.length === 0) {
      view.lines = arrayView(records).lines;
      fact(view.facts, words(key), plural(records.length, 'item'));
      continue;
    }
    fact(view.facts, words(key), scalarText(value));
  }
  return view;
}

function arrayView(rows: unknown[]): AoResultView {
  const view = emptyView();
  for (const [index, row] of list(rows).entries()) {
    const title = str(row, 'title') || str(row, 'name') || str(row, 'label') || str(row, 'id') || `item ${index + 1}`;
    const detail = join(str(row, 'state'), str(row, 'status'), str(row, 'kind'));
    view.lines.push({ key: `${index}:${title}`, title, detail, text: '', link: null });
  }
  view.outcome = plural(rows.length, 'item');
  return view;
}

/**
 * The reply of any AO tool as its row's body shows it, or null when the
 * reply is not JSON (plain text renders as it is) or is a remote run reply,
 * which renders through `remoteResultView`.
 */
export function aoResultView(
  server: string,
  tool: string,
  data: string,
  input: Input,
  names: AoToolNames,
): AoResultView | null {
  switch (server) {
    case THREAD_TOOLS_SERVER:
      return threadResultView(tool, data, input, names);
    case WORKFLOW_TOOLS_SERVER:
      return workflowResultView(tool, data, names);
    case BROWSER_TOOLS_SERVER:
      return browserResultView(tool, data);
    case REMOTE_TOOLS_SERVER:
      return remoteResultView(data) ? null : remoteOtherView(data);
    default:
      return null;
  }
}

/** Whether a request state is one the request will not leave. */
export function isSettledRequestState(state: string): boolean {
  return SETTLED_STATES.has(state);
}
